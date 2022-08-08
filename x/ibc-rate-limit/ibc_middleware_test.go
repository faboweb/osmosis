package ibc_rate_limit_test

import (
	"encoding/json"
	"fmt"
	sdk "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v3/modules/apps/transfer/types"
	clienttypes "github.com/cosmos/ibc-go/v3/modules/core/02-client/types"
	ibctesting "github.com/cosmos/ibc-go/v3/testing"
	"github.com/osmosis-labs/osmosis/v10/app"
	"github.com/osmosis-labs/osmosis/v10/app/apptesting"
	"github.com/osmosis-labs/osmosis/v10/x/ibc-rate-limit/testutil"
	"github.com/osmosis-labs/osmosis/v10/x/ibc-rate-limit/types"
	"github.com/stretchr/testify/suite"
	"testing"
)

type MiddlewareTestSuite struct {
	apptesting.KeeperTestHelper

	coordinator *ibctesting.Coordinator

	// testing chains used for convenience and readability
	chainA *osmosisibctesting.TestChain
	chainB *osmosisibctesting.TestChain
	path   *ibctesting.Path
}

func TestMiddlewareTestSuite(t *testing.T) {
	suite.Run(t, new(MiddlewareTestSuite))
}

func SetupTestingApp() (ibctesting.TestingApp, map[string]json.RawMessage) {
	osmosisApp := app.Setup(false)
	return osmosisApp, app.NewDefaultGenesisState()
}

func NewTransferPath(chainA, chainB *osmosisibctesting.TestChain) *ibctesting.Path {
	path := ibctesting.NewPath(chainA.TestChain, chainB.TestChain)
	path.EndpointA.ChannelConfig.PortID = ibctesting.TransferPort
	path.EndpointB.ChannelConfig.PortID = ibctesting.TransferPort
	path.EndpointA.ChannelConfig.Version = transfertypes.Version
	path.EndpointB.ChannelConfig.Version = transfertypes.Version
	return path
}

func (suite *MiddlewareTestSuite) SetupTest() {
	suite.Setup()
	ibctesting.DefaultTestingAppInit = SetupTestingApp
	suite.coordinator = ibctesting.NewCoordinator(suite.T(), 3)
	suite.chainA = &osmosisibctesting.TestChain{
		TestChain: suite.coordinator.GetChain(ibctesting.GetChainID(1)),
	}
	suite.chainB = &osmosisibctesting.TestChain{
		TestChain: suite.coordinator.GetChain(ibctesting.GetChainID(2)),
	}
	suite.path = NewTransferPath(suite.chainA, suite.chainB)
	suite.coordinator.Setup(suite.path)
}

func (suite *MiddlewareTestSuite) NewValidMessage(forward bool) sdk.Msg {
	var coins sdk.Coin
	var port, channel, accountFrom, accountTo string

	if forward {
		coins = sdk.NewCoin(sdk.DefaultBondDenom, sdk.NewInt(1))
		port = suite.path.EndpointA.ChannelConfig.PortID
		channel = suite.path.EndpointA.ChannelID
		accountFrom = suite.chainA.SenderAccount.GetAddress().String()
		accountTo = suite.chainB.SenderAccount.GetAddress().String()
	} else {
		//coinSentFromAToB := transfertypes.GetTransferCoin(path.EndpointB.ChannelConfig.PortID, path.EndpointB.ChannelID, sdk.DefaultBondDenom, sdk.NewInt(1))
		coins = transfertypes.GetTransferCoin(
			suite.path.EndpointB.ChannelConfig.PortID,
			suite.path.EndpointB.ChannelID,
			sdk.DefaultBondDenom,
			sdk.NewInt(1),
		)
		coins = sdk.NewCoin(sdk.DefaultBondDenom, sdk.NewInt(1))
		port = suite.path.EndpointB.ChannelConfig.PortID
		channel = suite.path.EndpointB.ChannelID
		accountFrom = suite.chainB.SenderAccount.GetAddress().String()
		accountTo = suite.chainA.SenderAccount.GetAddress().String()
	}

	timeoutHeight := clienttypes.NewHeight(0, 100)
	return transfertypes.NewMsgTransfer(
		port,
		channel,
		coins,
		accountFrom,
		accountTo,
		timeoutHeight,
		0,
	)
}

func (suite *MiddlewareTestSuite) ExecuteReceive(msg sdk.Msg) (string, error) {
	res, err := suite.chainB.SendMsgsNoCheck(msg)
	suite.Require().NoError(err)

	packet, err := ibctesting.ParsePacketFromEvents(res.GetEvents())
	suite.Require().NoError(err)

	err = suite.path.EndpointA.UpdateClient()
	suite.Require().NoError(err)

	res, err = suite.path.EndpointA.RecvPacketWithResult(packet)
	suite.Require().NoError(err)

	ack, err := ibctesting.ParseAckFromEvents(res.GetEvents())
	return string(ack), err
}

func (suite *MiddlewareTestSuite) AssertReceiveSucceeds(success bool, msg sdk.Msg) {
	ack, err := suite.ExecuteReceive(msg)
	if success {
		suite.Require().NoError(err)
		suite.Require().NotContains(string(ack), "error",
			"acknoledgment is an error")
	} else {
		suite.Require().Contains(string(ack), "error",
			"acknoledgment is not an error")
		suite.Require().Contains(string(ack), types.RateLimitExceededMsg,
			"acknoledgment error is not of the right type")
	}
}

func (suite *MiddlewareTestSuite) AssertSendSucceeds(success bool, msg sdk.Msg) {
	_, err := suite.chainA.SendMsgsNoCheck(msg)
	if success {
		suite.Require().NoError(err, "IBC send failed. Expected success. %s", err)
	} else {
		suite.Require().Error(err, "IBC send succeeded. Expected failure")
		suite.ErrorContains(err, types.RateLimitExceededMsg)
	}
}

func (suite *MiddlewareTestSuite) TestSendTransferWithoutRateLimitingContract() {
	suite.AssertSendSucceeds(true, suite.NewValidMessage(true))
}

func (suite *MiddlewareTestSuite) TestReceiveTransferWithoutRateLimitingContract() {
	suite.AssertReceiveSucceeds(true, suite.NewValidMessage(false))
}

func (suite *MiddlewareTestSuite) TestSendTransferWithNewRateLimitingContract() {
	fmt.Println("Running this once")
	suite.chainA.StoreContractCode(&suite.Suite)
	addr := suite.chainA.InstantiateContract(&suite.Suite)
	addrJson, _ := addr.MarshalJSON()
	fmt.Println(string(addrJson))
	suite.AssertSendSucceeds(true, suite.NewValidMessage(true))
}
