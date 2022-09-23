package keeper

import (
	"fmt"

	gogotypes "github.com/gogo/protobuf/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	"github.com/osmosis-labs/osmosis/v12/osmoutils"
	"github.com/osmosis-labs/osmosis/v12/x/gamm/pool-models/balancer"
	"github.com/osmosis-labs/osmosis/v12/x/gamm/types"
)

func (k Keeper) MarshalPool(pool types.PoolI) ([]byte, error) {
	return k.cdc.MarshalInterface(pool)
}

func (k Keeper) UnmarshalPool(bz []byte) (types.PoolI, error) {
	var acc types.PoolI
	return acc, k.cdc.UnmarshalInterface(bz, &acc)
}

// GetPoolAndPoke returns a PoolI based on it's identifier if one exists. Prior
// to returning the pool, the weights of the pool are updated via PokePool.
// TODO: Consider rename to GetPool due to downstream API confusion.
func (k Keeper) GetPoolAndPoke(ctx sdk.Context, poolId uint64) (types.PoolI, error) {
	store := ctx.KVStore(k.storeKey)
	poolKey := types.GetKeyPrefixPools(poolId)
	if !store.Has(poolKey) {
		return nil, types.PoolDoesNotExistError{PoolId: poolId}
	}

	bz := store.Get(poolKey)

	pool, err := k.UnmarshalPool(bz)
	if err != nil {
		return nil, err
	}

	pool.PokePool(ctx.BlockTime())

	return pool, nil
}

// Get pool and check if the pool is active, i.e. allowed to be swapped against.
func (k Keeper) getPoolForSwap(ctx sdk.Context, poolId uint64) (types.PoolI, error) {
	pool, err := k.GetPoolAndPoke(ctx, poolId)
	if err != nil {
		return &balancer.Pool{}, err
	}

	if !pool.IsActive(ctx) {
		return &balancer.Pool{}, sdkerrors.Wrapf(types.ErrPoolLocked, "swap on inactive pool")
	}
	return pool, nil
}

func (k Keeper) iterator(ctx sdk.Context, prefix []byte) sdk.Iterator {
	store := ctx.KVStore(k.storeKey)
	return sdk.KVStorePrefixIterator(store, prefix)
}

func (k Keeper) GetPoolsAndPoke(ctx sdk.Context) (res []types.PoolI, err error) {
	iter := k.iterator(ctx, types.KeyPrefixPools)
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		bz := iter.Value()

		pool, err := k.UnmarshalPool(bz)
		if err != nil {
			return nil, err
		}

		pool.PokePool(ctx.BlockTime())
		res = append(res, pool)
	}

	return res, nil
}

func (k Keeper) setPool(ctx sdk.Context, pool types.PoolI) error {
	bz, err := k.MarshalPool(pool)
	if err != nil {
		return err
	}

	store := ctx.KVStore(k.storeKey)
	poolKey := types.GetKeyPrefixPools(pool.GetId())
	store.Set(poolKey, bz)

	return nil
}

func (k Keeper) DeletePool(ctx sdk.Context, poolId uint64) error {
	store := ctx.KVStore(k.storeKey)
	poolKey := types.GetKeyPrefixPools(poolId)
	if !store.Has(poolKey) {
		return fmt.Errorf("pool with ID %d does not exist", poolId)
	}

	store.Delete(poolKey)
	return nil
}

// CleanupPools destructs a pool and refunds all the assets according to
// the shares held by the accounts. CleanupPools should not be called during
// the chain execution time, as it iterates the entire account balances.
//
// CONTRACT: All locks on this pool share must be unlocked prior to execution. Use LockupKeeper.ForceUnlock
// on remaining locks before calling this function.
func (k Keeper) CleanupPools(ctx sdk.Context, poolIds []uint64) (err error) {
	// we use maps here because we can't alter the state of pool directly
	type poolInfo struct {
		address     sdk.AccAddress
		totalShares sdk.Int
		liquidity   sdk.Coins
	}

	poolInfos := make(map[string]poolInfo)

	for _, poolId := range poolIds {
		pool, err := k.GetPoolAndPoke(ctx, poolId)
		if err != nil {
			return err
		}
		shareDenom := types.GetPoolShareDenom(poolId)

		poolInfos[shareDenom] = poolInfo{
			pool.GetAddress(),
			pool.GetTotalShares(),
			pool.GetTotalPoolLiquidity(ctx),
		}
	}

	// first iterate through the share holders and burn them
	k.bankKeeper.IterateAllBalances(ctx, func(addr sdk.AccAddress, coin sdk.Coin) (stop bool) {
		// skip to next iteration if the coin amount is zero
		if coin.Amount.IsZero() {
			return
		}

		// skip to next iteration if this coin is not a pool share
		pool, ok := poolInfos[coin.Denom]
		if !ok {
			return
		}

		totalShares := pool.totalShares
		poolLiquidity := pool.liquidity
		poolAddress := pool.address

		// Burn the share tokens
		err = k.bankKeeper.SendCoinsFromAccountToModule(ctx, addr, types.ModuleName, sdk.Coins{coin})
		if err != nil {
			return true
		}

		err = k.bankKeeper.BurnCoins(ctx, types.ModuleName, sdk.Coins{coin})
		if err != nil {
			return true
		}

		// Refund assets
		// lpShareEquivalentTokens = (amount in pool) * (your shares) / (total shares)
		for _, asset := range poolLiquidity {
			// should we just give the remaining liquidity to the pool address?
			lpShareEquivalentTokens := asset.Amount.Mul(coin.Amount).Quo(totalShares)
			if lpShareEquivalentTokens.IsZero() {
				continue
			}
			poolAssetToReturn := sdk.NewCoin(asset.Denom, lpShareEquivalentTokens)
			poolLiquidity = poolLiquidity.Sub(sdk.Coins{poolAssetToReturn})

			err = k.bankKeeper.SendCoins(
				ctx, poolAddress, addr, sdk.Coins{poolAssetToReturn})
			if err != nil {
				return true
			}
		}

		totalShares = totalShares.Sub(coin.Amount)

		// save updated state of pool in map for next iteration
		poolInfos[coin.Denom] = poolInfo{
			poolAddress,
			totalShares,
			poolLiquidity,
		}
		return false
	})

	if err != nil {
		return err
	}

	for _, poolId := range poolIds {
		pool, err := k.GetPoolAndPoke(ctx, poolId)
		if err != nil {
			return err
		}
		shareDenom := types.GetPoolShareDenom(poolId)
		poolInfo := poolInfos[shareDenom]
		// sanity check that we have no remaining shares
		if !poolInfo.totalShares.IsZero() {
			return fmt.Errorf("pool %d still has liquidity after cleanup", poolId)
		}

		// check that we have no remaining balances in the pool
		coins := k.bankKeeper.GetAllBalances(ctx, pool.GetAddress())
		if !coins.IsZero() {
			return fmt.Errorf("pool %d still has remaining balance after cleanup", poolId)
		}

		// delete the pool once sanity check has been dones
		err = k.DeletePool(ctx, pool.GetId())
		if err != nil {
			return err
		}
	}

	return nil
}

// GetPoolDenom retrieves the pool based on PoolId and
// returns the coin denoms that it holds.
func (k Keeper) GetPoolDenoms(ctx sdk.Context, poolId uint64) ([]string, error) {
	pool, err := k.GetPoolAndPoke(ctx, poolId)
	if err != nil {
		return nil, err
	}

	denoms := osmoutils.CoinsDenoms(pool.GetTotalPoolLiquidity(ctx))
	return denoms, err
}

// setNextPoolId sets next pool Id.
func (k Keeper) setNextPoolId(ctx sdk.Context, poolId uint64) {
	store := ctx.KVStore(k.storeKey)
	bz := k.cdc.MustMarshal(&gogotypes.UInt64Value{Value: poolId})
	store.Set(types.KeyNextGlobalPoolId, bz)
}

// GetNextPoolId returns the next pool Id.
func (k Keeper) GetNextPoolId(ctx sdk.Context) uint64 {
	var nextPoolId uint64
	store := ctx.KVStore(k.storeKey)

	bz := store.Get(types.KeyNextGlobalPoolId)
	if bz == nil {
		panic(fmt.Errorf("pool has not been initialized -- Should have been done in InitGenesis"))
	} else {
		val := gogotypes.UInt64Value{}

		err := k.cdc.Unmarshal(bz, &val)
		if err != nil {
			panic(err)
		}

		nextPoolId = val.GetValue()
	}
	return nextPoolId
}

// getNextPoolIdAndIncrement returns the next pool Id, and increments the corresponding state entry.
func (k Keeper) getNextPoolIdAndIncrement(ctx sdk.Context) uint64 {
	nextPoolId := k.GetNextPoolId(ctx)
	k.setNextPoolId(ctx, nextPoolId+1)
	return nextPoolId
}
