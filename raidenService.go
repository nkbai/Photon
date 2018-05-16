package smartraiden

import (
	"crypto/ecdsa"
	"errors"

	"fmt"

	"path/filepath"

	"time"

	"math/rand"

	"sync"

	"sync/atomic"

	"math/big"

	"strings"

	"os"

	"runtime/debug"

	"github.com/SmartMeshFoundation/SmartRaiden/blockchain"
	"github.com/SmartMeshFoundation/SmartRaiden/channel"
	"github.com/SmartMeshFoundation/SmartRaiden/encoding"
	"github.com/SmartMeshFoundation/SmartRaiden/log"
	"github.com/SmartMeshFoundation/SmartRaiden/models"
	"github.com/SmartMeshFoundation/SmartRaiden/network"
	"github.com/SmartMeshFoundation/SmartRaiden/network/rpc"
	"github.com/SmartMeshFoundation/SmartRaiden/network/rpc/fee"
	"github.com/SmartMeshFoundation/SmartRaiden/params"
	"github.com/SmartMeshFoundation/SmartRaiden/rerr"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer/mediated_transfer"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer/mediated_transfer/initiator"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer/mediated_transfer/mediator"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer/mediated_transfer/target"
	"github.com/SmartMeshFoundation/SmartRaiden/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/theckman/go-flock"
)

/*
message sent complete notification
*/
type ProtocolMessage struct {
	receiver common.Address
	Message  encoding.Messager
}

//return true to ignore this message,otherwise continue to process
type SecretRequestPredictor func(msg *encoding.SecretRequest) (ignore bool)

//return true this listener should not be called next time
type RevealSecretListener func(msg *encoding.RevealSecret) (remove bool)

//return true this listener should not be called next time
type ReceivedMediatedTrasnferListener func(msg *encoding.MediatedTransfer) (remove bool)

//return true this listener should not be called next time
type SentMediatedTransferListener func(msg *encoding.MediatedTransfer) (remove bool)

// A Raiden node.
/*
most of raidenService's member is not thread safe, and should not visit outside the loop method.
*/
type RaidenService struct {
	Chain              *rpc.BlockChainService
	Registry           *rpc.RegistryProxy
	RegistryAddress    common.Address
	PrivateKey         *ecdsa.PrivateKey
	Transport          network.Transporter
	Discovery          network.Discovery
	Config             *params.Config
	Protocol           *network.RaidenProtocol
	NodeAddress        common.Address
	Token2ChannelGraph map[common.Address]*network.ChannelGraph
	//Token2ConnectionsManager todo fix later
	//swapkey_to_tokenswap
	//swapkey_to_greenlettask
	Manager2Token            map[common.Address]common.Address
	Identifier2StateManagers map[uint64][]*transfer.StateManager
	Identifier2Results       map[uint64][]*network.AsyncResult
	SwapKey2TokenSwap        map[SwapKey]*TokenSwap
	Tokens2ConnectionManager map[common.Address]*ConnectionManager //how to save and restore for token swap? todo fix it
	/*
				   This is a map from a hashlock to a list of channels, the same
			         hashlock can be used in more than one token (for tokenswaps), a
			         channel should be removed from this list only when the Lock is
			         released/withdrawn but not when the secret is registered.
		TODO remove this,this design is very weird
	*/
	Token2Hashlock2Channels  map[common.Address]map[common.Hash][]*channel.Channel //Multithread
	MessageHandler           *RaidenMessageHandler
	StateMachineEventHandler *StateMachineEventHandler
	BlockChainEvents         *blockchain.Events
	AlarmTask                *blockchain.AlarmTask
	db                       *models.ModelDB
	FileLocker               *flock.Flock
	SnapshortDir             string
	BlockNumber              *atomic.Value
	/*
		new block event
	*/
	BlockNumberChan chan int64
	/*
		chan for user request
	*/
	UserReqChan                 chan *ApiReq
	ProtocolMessageSendComplete chan *ProtocolMessage
	RoutesTask                  *RoutesTask
	FeePolicy                   fee.FeeCharger //Mediation fee
	/*
		these four maps designed for token swap,but it can be extended for purpose usage.
		for example:
		cross chain.
	*/
	SecretRequestPredictorMap map[common.Hash]SecretRequestPredictor //for tokenswap
	RevealSecretListenerMap   map[common.Hash]RevealSecretListener   //for tokenswap
	/*
		important!:
			we must valid the mediated transfer is valid or not first, then to test  if this mediated transfer matchs any token swap.
	*/
	ReceivedMediatedTrasnferListenerMap map[*ReceivedMediatedTrasnferListener]bool //for tokenswap
	SentMediatedTransferListenerMap     map[*SentMediatedTransferListener]bool     //for tokenswap
	HealthCheckMap                      map[common.Address]bool
}

func NewRaidenService(chain *rpc.BlockChainService, privateKey *ecdsa.PrivateKey, transport network.Transporter,
	discover network.DiscoveryInterface, config *params.Config) (srv *RaidenService) {
	if config.SettleTimeout < params.NettingChannelSettleTimeoutMin || config.SettleTimeout > params.NettingChannelSettleTimeoutMax {
		log.Error(fmt.Sprintf("settle timeout must be in range %d-%d",
			params.NettingChannelSettleTimeoutMin, params.NettingChannelSettleTimeoutMax))
		utils.SystemExit(1)
	}
	srv = &RaidenService{
		Chain:                               chain,
		Registry:                            chain.Registry(chain.RegistryAddress),
		RegistryAddress:                     chain.RegistryAddress,
		PrivateKey:                          privateKey,
		Config:                              config,
		NodeAddress:                         crypto.PubkeyToAddress(privateKey.PublicKey),
		Token2ChannelGraph:                  make(map[common.Address]*network.ChannelGraph),
		Manager2Token:                       make(map[common.Address]common.Address),
		Identifier2StateManagers:            make(map[uint64][]*transfer.StateManager),
		Identifier2Results:                  make(map[uint64][]*network.AsyncResult),
		Token2Hashlock2Channels:             make(map[common.Address]map[common.Hash][]*channel.Channel),
		SwapKey2TokenSwap:                   make(map[SwapKey]*TokenSwap),
		Tokens2ConnectionManager:            make(map[common.Address]*ConnectionManager),
		AlarmTask:                           blockchain.NewAlarmTask(chain.Client),
		BlockChainEvents:                    blockchain.NewBlockChainEvents(chain.Client, chain.RegistryAddress),
		BlockNumberChan:                     make(chan int64, 1),
		UserReqChan:                         make(chan *ApiReq, 10),
		BlockNumber:                         new(atomic.Value),
		ProtocolMessageSendComplete:         make(chan *ProtocolMessage, 10),
		SecretRequestPredictorMap:           make(map[common.Hash]SecretRequestPredictor),
		RevealSecretListenerMap:             make(map[common.Hash]RevealSecretListener),
		ReceivedMediatedTrasnferListenerMap: make(map[*ReceivedMediatedTrasnferListener]bool),
		SentMediatedTransferListenerMap:     make(map[*SentMediatedTransferListener]bool),
		FeePolicy:                           &ConstantFeePolicy{},
		HealthCheckMap:                      make(map[common.Address]bool),
	}
	var err error
	srv.MessageHandler = NewRaidenMessageHandler(srv)
	srv.StateMachineEventHandler = NewStateMachineEventHandler(srv)
	srv.Protocol = network.NewRaidenProtocol(transport, discover, privateKey, srv)
	srv.db, err = models.OpenDb(config.DataBasePath)
	if err != nil {
		log.Error("open db error")
		utils.SystemExit(1)
	}
	srv.Protocol.SetReceivedMessageSaver(NewAckHelper(srv.db))
	/*
		only one instance for one data directory
	*/
	srv.FileLocker = flock.NewFlock(config.DataBasePath + ".flock.Lock")
	locked, err := srv.FileLocker.TryLock()
	if err != nil || !locked {
		log.Error(fmt.Sprint("another instance already running at %s", config.DataBasePath))
		utils.SystemExit(1)
	}
	srv.SnapshortDir = filepath.Join(config.DataBasePath)
	err = discover.Register(srv.NodeAddress, srv.Config.ExternIp, srv.Config.ExternPort)
	if err != nil {
		log.Error(fmt.Sprintf("register discover endpoint error:%s", err))
		utils.SystemExit(1)
	}
	log.Info("node discovery register complete...")
	//srv.Start()
	//start routes detect task
	srv.RoutesTask = NewRoutesTask(srv.Protocol, srv.Protocol)
	return srv
}

// Start the node.
func (this *RaidenService) Start() {
	lastHandledBlockNumber := this.db.GetLatestBlockNumber()
	this.AlarmTask.Start()
	this.RoutesTask.Start()
	//must have a valid blocknumber before any transfer operation
	this.BlockNumber.Store(this.AlarmTask.LastBlockNumber)
	this.AlarmTask.RegisterCallback(func(number int64) error {
		this.db.SaveLatestBlockNumber(number)
		return this.setBlockNumber(number)
	})
	/*
		events before lastHandledBlockNumber must have been processed, so we start from  lastHandledBlockNumber-1
	*/
	err := this.BlockChainEvents.Start(lastHandledBlockNumber)
	if err != nil {
		log.Error(fmt.Sprintf("Events listener error %v", err))
		utils.SystemExit(1)
	}
	/*
			  Registry registration must start *after* the alarm task, this avoid
		         corner cases were the registry is queried in block A, a new block B
		         is mined, and the alarm starts polling at block C.
	*/
	this.RegisterRegistry()
	err = this.RestoreSnapshot()
	if err != nil {
		log.Error(fmt.Sprintf("restore from snapshot error : %v\n you can delete all the database %s to run. but all your trade will lost!!", err, this.Config.DataBasePath))
		utils.SystemExit(1)
	}
	this.Protocol.Start()
	this.StartNeighboursHealthCheck()
	go func() {
		if this.Config.ConditionQuit.RandomQuit {
			go func() {
				isPrime := func(value int) bool {
					if value <= 3 {
						return value >= 2
					}
					if value%2 == 0 || value%3 == 0 {
						return false
					}
					for i := 5; i*i <= value; i += 6 {
						if value%i == 0 || value%(i+2) == 0 {
							return false
						}
					}
					return true
				}
				for {
					/*
							Random sleep is no more than five seconds. If the number of dormancy milliseconds is prime,
						    it will exit directly. This probability is probably 13%.
					*/
					n := utils.NewRandomInt(5000)
					time.Sleep(time.Duration(n) * time.Millisecond)
					if isPrime(n) {
						panic("random quit")
					}
				}
			}()
		}
		this.loop()
	}()
}

//Stop the node.
func (this *RaidenService) Stop() {
	log.Info("raiden service stop...")
	this.AlarmTask.Stop()
	this.RoutesTask.Stop()
	this.Protocol.StopAndWait()
	this.BlockChainEvents.Stop()
	this.SaveSnapshot()
	time.Sleep(100 * time.Millisecond) // let other goroutines quit
	this.db.CloseDB()
	//anther instance cann run now
	this.FileLocker.Unlock()
	log.Info("raiden service stop ok...")
}

/*
main loop of this raiden nodes
process  events below:
1. request from user
2. event from blockchain
3. message from other nodes.
*/
func (this *RaidenService) loop() {
	var err error
	var ok bool
	var m *network.MessageToRaiden
	var st transfer.StateChange
	var blockNumber int64
	var req *ApiReq
	var sentMessage *ProtocolMessage
	var routestask *RoutesToDetect
	for {
		select {
		//message from other nodes
		case m, ok = <-this.Protocol.ReceivedMessageChan:
			if ok {
				err = this.MessageHandler.OnMessage(m.Msg, m.EchoHash)
				if err != nil {
					log.Error(fmt.Sprintf("MessageHandler.OnMessage %v", err))
				}
				this.Protocol.ReceivedMessageResultChan <- err
			} else {
				log.Info("Protocol.ReceivedMessageChan closed")
				return
			}
			// contract events from block chain
		case st, ok = <-this.BlockChainEvents.StateChangeChannel:
			if ok {
				err = this.StateMachineEventHandler.OnBlockchainStateChange(st)
				if err != nil {
					log.Error("StateMachineEventHandler.OnBlockchainStateChange", err)
				}
			} else {
				log.Info("Events.StateChangeChannel closed")
				return
			}
			// new block event, it's the timer of raiden
		case blockNumber, ok = <-this.BlockNumberChan:
			if ok {
				this.handleBlockNumber(blockNumber)
			} else {
				log.Info("BlockNumberChan closed")
				return
			}
		//user's request
		case req, ok = <-this.UserReqChan:
			if ok {
				this.handleReq(req)
			} else {
				log.Info("req closed")
				return
			}
			//i have sent a message complete
		case sentMessage, ok = <-this.ProtocolMessageSendComplete:
			if ok {
				this.handleSentMessage(sentMessage)
			} else {
				log.Info("ProtocolMessageSendComplete closed")
				return
			}
			//before send a transfer, we would better detect if neighbors are online or not.
		case routestask, ok = <-this.RoutesTask.TaskResult:
			if ok {
				this.handleRoutesTask(routestask)
			} else {
				log.Info("RoutesTask.TaskResult closed")
				return
			}
		}
	}
}

//for init
func (this *RaidenService) RegisterRegistry() {
	mgrs, err := this.Chain.GetAllChannelManagers()
	if err != nil {
		log.Error(fmt.Sprintf("RegisterRegistry err:%s", err))
		utils.SystemExit(1)
	}
	for _, mgr := range mgrs {
		err = this.RegisterChannelManager(mgr.Address)
		if err != nil {
			log.Error(fmt.Sprintf("RegisterChannelManager err:%s", err))
			utils.SystemExit(1)
		}
	}
}

/*
quering my channel details on blockchain
i'm one of the channel participants
*/
func (this *RaidenService) getChannelDetail(tokenAddress common.Address, proxy *rpc.NettingChannelContractProxy) *network.ChannelDetails {
	addr1, b1, addr2, b2, _ := proxy.AddressAndBalance()
	var ourAddr, partnerAddr common.Address
	var ourBalance, partnerBalance *big.Int
	if addr1 == this.NodeAddress {
		ourAddr = addr1
		partnerAddr = addr2
		ourBalance = b1
		partnerBalance = b2
	} else {
		ourAddr = addr2
		partnerAddr = addr1
		ourBalance = b2
		partnerBalance = b1
	}
	ourState := channel.NewChannelEndState(ourAddr, ourBalance, nil, transfer.EmptyMerkleTreeState)
	partenerState := channel.NewChannelEndState(partnerAddr, partnerBalance, nil, transfer.EmptyMerkleTreeState)
	channelAddress := proxy.Address
	registerChannelForHashlock := func(channel *channel.Channel, hashlock common.Hash) {
		this.RegisterChannelForHashlock(tokenAddress, channel, hashlock)
	}
	externState := channel.NewChannelExternalState(registerChannelForHashlock, proxy, channelAddress, this.Chain, this.db)
	channelDetail := &network.ChannelDetails{
		ChannelAddress:    channelAddress,
		OurState:          ourState,
		PartenerState:     partenerState,
		ExternState:       externState,
		BlockChainService: this.Chain,
		RevealTimeout:     this.Config.RevealTimeout,
	}
	channelDetail.SettleTimeout, _ = externState.NettingChannel.SettleTimeout()
	return channelDetail
}

func (this *RaidenService) setBlockNumber(blocknumber int64) error {
	this.BlockNumberChan <- blocknumber
	return nil
}

/*
block chain tick,
it's the core of HTLC
*/
func (this *RaidenService) handleBlockNumber(blocknumber int64) error {
	statechange := &transfer.BlockStateChange{blocknumber}
	this.BlockNumber.Store(blocknumber)
	/*
		todo when to remove statemanager ?
			when currentState==nil && StateManager.ManagerState!=StateManagerStateInit ,should delete this statemanager.
	*/
	this.StateMachineEventHandler.LogAndDispatchToAllTasks(statechange)
	for _, cg := range this.CloneToken2ChannelGraph() {
		for _, channel := range cg.ChannelAddress2Channel {
			this.StateMachineEventHandler.ChannelStateTransition(channel, statechange)
		}
	}

	return nil
}
func (this *RaidenService) GetBlockNumber() int64 {
	return this.BlockNumber.Load().(int64)
}

func (this *RaidenService) FindChannelByAddress(nettingChannelAddress common.Address) (*channel.Channel, error) {
	for _, g := range this.Token2ChannelGraph {
		ch := g.GetChannelAddress2Channel(nettingChannelAddress)
		if ch != nil {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("unknown channel %s", nettingChannelAddress)
}

/*
Send `message` to `recipient` using the raiden protocol.

       The protocol will take care of resending the message on a given
       interval until an Acknowledgment is received or a given number of
       tries.
*/
func (this *RaidenService) SendAsync(recipient common.Address, msg encoding.SignedMessager) error {
	if recipient == this.NodeAddress {
		log.Error(fmt.Sprintf("this must be a bug ,sending message to it self"))
	}
	revealSecretMessage, ok := msg.(*encoding.RevealSecret)
	if ok && revealSecretMessage != nil {
		srs := models.NewSentRevealSecret(revealSecretMessage, recipient)
		this.db.NewSentRevealSecret(srs)
		if msg.Tag() == nil {
			msg.SetTag(&transfer.MessageTag{
				EchoHash:          srs.EchoHash,
				IsASendingMessage: true,
			})
		} else {
			messageTag := msg.Tag().(*transfer.MessageTag)
			if messageTag.EchoHash != srs.EchoHash {
				panic("reveal secret's echo hash not equal")
			}
		}
	}
	mtr, ok := msg.(*encoding.MediatedTransfer)
	if ok && mtr != nil {
		for f, _ := range this.SentMediatedTransferListenerMap {
			remove := (*f)(mtr)
			if remove {
				delete(this.SentMediatedTransferListenerMap, f)
			}
		}
	}
	result := this.Protocol.SendAsync(recipient, msg)
	go func() {
		<-result.Result //always success
		this.ProtocolMessageSendComplete <- &ProtocolMessage{
			receiver: recipient,
			Message:  msg,
		}
	}()
	return nil
}

/*
Send `message` to `recipient` and wait for the response or `timeout`.

       Args:
           recipient (address): The address of the node that will receive the
               message.
           message: The transfer message.
           timeout (float): How long should we wait for a response from `recipient`.

       Returns:
           None: If the wait timed out
           object: The result from the event
*/
func (this *RaidenService) SendAndWait(recipient common.Address, message encoding.SignedMessager, timeout time.Duration) error {
	return this.Protocol.SendAndWait(recipient, message, timeout)
}

/*
Register the secret with any channel that has a hashlock on it.

       This must search through all channels registered for a given hashlock
       and ignoring the tokens. Useful for refund transfer, split transfer,
       and token swaps.

       Raises:
           TypeError: If secret is unicode data.
*/
func (this *RaidenService) RegisterSecret(secret common.Hash) {
	hashlock := utils.Sha3(secret[:])
	revealSecretMessage := encoding.NewRevealSecret(secret)
	revealSecretMessage.Sign(this.PrivateKey, revealSecretMessage)
	for _, hashchannel := range this.Token2Hashlock2Channels {
		for _, ch := range hashchannel[hashlock] {
			err := ch.RegisterSecret(secret)
			if err != nil {
				log.Error(fmt.Sprintf("RegisterSecret %s to channel %s  err: %s",
					utils.HPex(secret), utils.APex2(ch.MyAddress), err))
			}
			this.ConditionQuit("BeforeSendRevealSecret")
			this.db.UpdateChannelNoTx(channel.NewChannelSerialization(ch))
			//The protocol ignores duplicated messages.
			//make sure not send the same instance multi times.
			this.SendAsync(ch.PartnerState.Address, encoding.CloneRevealSecret(revealSecretMessage))
		}
	}
}

func (this *RaidenService) RegisterChannelForHashlock(tokenAddress common.Address,
	netchannel *channel.Channel, hashlock common.Hash) {
	channelsRegistered := this.Token2Hashlock2Channels[tokenAddress][hashlock]
	found := false
	for _, c := range channelsRegistered {
		//To determine whether the two channel objects are equal, we simply use the address to identify.
		if c.ExternState.ChannelAddress == netchannel.ExternState.ChannelAddress {
			found = true
			break
		}
	}
	if !found {
		hashLock2Channels, ok := this.Token2Hashlock2Channels[tokenAddress]
		if !ok {
			hashLock2Channels = make(map[common.Hash][]*channel.Channel)
			this.Token2Hashlock2Channels[tokenAddress] = hashLock2Channels
		}
		channelsRegistered = append(channelsRegistered, netchannel)
		this.Token2Hashlock2Channels[tokenAddress][hashlock] = channelsRegistered
	}
}

/*
Unlock/Witdraws locks, register the secret, and send Secret
       messages as necessary.

       This function will:
           - Unlock the locks created by this node and send a Secret message to
           the corresponding partner so that she can withdraw the token.
           - Withdraw the Lock from sender.
           - Register the secret for the locks received and reveal the secret
           to the senders


       Note:
           The channel needs to be registered with
           `raiden.register_channel_for_hashlock`.
//todo 需要再次确认, refund 处理流程有无问题,以及相关细节必须审核.
*/
func (this *RaidenService) HandleSecret(identifier uint64, tokenAddress common.Address, secret common.Hash,
	partnerSecretMessage *encoding.Secret, hashlock common.Hash) (err error) {
	/*
	   handling the secret needs to:
	         - unlock the token for all `forward_channel` (the current one
	           and the ones that failed with a refund)
	         - send a message to each of the forward nodes allowing them
	           to withdraw the token
	         - register the secret for the `originating_channel` so that a
	           proof can be made, if necessary
	         - reveal the secret to the `sender` node (otherwise we
	           cannot withdraw the token)
	*/
	channelsList := this.Token2Hashlock2Channels[tokenAddress][hashlock]
	var channelsToRemove []*channel.Channel
	revealSecretMessage := encoding.NewRevealSecret(secret)
	revealSecretMessage.Sign(this.PrivateKey, revealSecretMessage)
	type MsgToSend struct {
		receiver common.Address
		msg      encoding.SignedMessager
	}
	var messagesToSend []*MsgToSend
	log.Trace(fmt.Sprintf("channelsList for %s =%#v", utils.HPex(hashlock), channelsList))
	for _, ch := range channelsList { //Dealing with reused nodes in indirect transactions.
		//unlock a pending Lock
		log.Trace(fmt.Sprintf("process channel %s-%s", utils.APex2(ch.OurState.Address), utils.APex2((ch.PartnerState.Address))))
		if ch.OurState.IsKnown(hashlock) {
			var secretMsg *encoding.Secret
			secretMsg, err = ch.CreateSecret(identifier, secret)
			if err != nil {
				return err
			}
			secretMsg.Sign(this.PrivateKey, secretMsg)
			//balance proof,Complete this transaction and receive repeated revealsecret,But secretmsg will only be sent once.
			err = ch.RegisterTransfer(this.GetBlockNumber(), secretMsg)
			if err != nil {
				return
			}
			messagesToSend = append(messagesToSend, &MsgToSend{ch.PartnerState.Address, secretMsg})
			channelsToRemove = append(channelsToRemove, ch)
		} else if ch.PartnerState.IsKnown(hashlock) {
			//withdraw a pending Lock
			if partnerSecretMessage != nil {
				msg := partnerSecretMessage
				isBalanceProof := msg.Sender == ch.PartnerState.Address && msg.Channel == ch.MyAddress
				if isBalanceProof {
					err = ch.RegisterTransfer(this.GetBlockNumber(), msg)
					if err != nil {
						return
					}
					channelsToRemove = append(channelsToRemove, ch)
				} else {
					err = ch.RegisterSecret(secret)
					if err != nil {
						return
					}
					this.ConditionQuit("BeforeSendRevealSecret")
					this.db.UpdateChannelNoTx(channel.NewChannelSerialization(ch))
					messagesToSend = append(messagesToSend, &MsgToSend{ch.PartnerState.Address, encoding.CloneRevealSecret(revealSecretMessage)})
				}
			} else {
				err = ch.RegisterSecret(secret)
				if err != nil {
					return
				}
				this.ConditionQuit("BeforeSendRevealSecret")
				this.db.UpdateChannelNoTx(channel.NewChannelSerialization(ch))
				messagesToSend = append(messagesToSend, &MsgToSend{ch.PartnerState.Address, encoding.CloneRevealSecret(revealSecretMessage)})
			}
		} else {
			/*
				todo reimplement HandleSecret
			*/
			log.Warn("Channel is registered for a given Lock but the Lock is not contained in it. can be ignored when I'm a mediated node")
		}

	}

	for _, ch := range channelsToRemove {
		//channels_list.remove(channel)
		for k, ch2 := range channelsList {
			if ch2 == ch {
				//to remove
				channelsList = append(channelsList[:k], channelsList[k+1:]...)
				break
			}
		}
	}
	if len(channelsList) == 0 {
		delete(this.Token2Hashlock2Channels[tokenAddress], hashlock)
	} else {
		this.Token2Hashlock2Channels[tokenAddress][hashlock] = channelsList
	}
	// send the messages last to avoid races
	for _, msg := range messagesToSend {
		err = this.SendAsync(msg.receiver, msg.msg)
		if err != nil {
			return
		}
	}
	return
}

func (this *RaidenService) ChannelManagerIsRegistered(manager common.Address) bool {
	_, ok := this.Manager2Token[manager]
	return ok
}
func (this *RaidenService) RegisterChannelManager(managerAddress common.Address) (err error) {
	manager := this.Chain.Manager(managerAddress)
	channels, err := manager.NettingChannelByAddress(this.NodeAddress)
	if err != nil {
		return
	}
	tokenAddress, _ := manager.TokenAddress()
	edgeList, _ := manager.GetChannelsParticipants()
	var channelsDetails []*network.ChannelDetails
	for _, ch := range channels {
		d := this.getChannelDetail(tokenAddress, ch)
		channelsDetails = append(channelsDetails, d)
	}
	graph := network.NewChannelGraph(this.NodeAddress, managerAddress, tokenAddress, edgeList, channelsDetails)
	this.Manager2Token[managerAddress] = tokenAddress
	this.Token2ChannelGraph[tokenAddress] = graph
	this.Tokens2ConnectionManager[tokenAddress] = NewConnectionManager(this, tokenAddress)
	//new token, save to db
	err = this.db.AddToken(tokenAddress, managerAddress)
	if err != nil {
		log.Error(err.Error())
	}
	err = this.db.UpdateTokenNodes(tokenAddress, graph.AllNodes())
	if err != nil {
		log.Error(err.Error())
	}
	// we need restore channel status from database after restart...
	//for _, c := range graph.ChannelAddress2Channel {
	//	err = this.db.UpdateChannelNoTx(channel.NewChannelSerialization(c))
	//	if err != nil {
	//		log.Info(err.Error())
	//	}
	//}

	return
}

/*
found new channel on blockchain when running...
*/
func (this *RaidenService) RegisterNettingChannel(tokenAddress, channelAddress common.Address) {
	nettingChannel, err := this.Chain.NettingChannel(channelAddress)
	if err != nil {
		log.Error("try to RegisterNettingChannel not exist channel %s", channelAddress)
	}
	detail := this.getChannelDetail(tokenAddress, nettingChannel)
	graph := this.GetToken2ChannelGraph(tokenAddress)
	err = graph.AddChannel(detail)
	if err != nil {
		log.Error(err.Error())
		return
	}
	err = this.db.NewChannel(channel.NewChannelSerialization(graph.ChannelAddress2Channel[channelAddress]))
	if err != nil {
		log.Error(err.Error())
		return
	}
	return
}
func (this *RaidenService) ConnectionManagerForToken(tokenAddress common.Address) (*ConnectionManager, error) {
	mgr, ok := this.Tokens2ConnectionManager[tokenAddress]
	if ok {
		return mgr, nil
	}
	return nil, rerr.InvalidAddress(fmt.Sprintf("token %s is not registered", utils.APex(tokenAddress)))
}
func (this *RaidenService) LeaveAllTokenNetworksAsync() *network.AsyncResult {
	var leaveResults []*network.AsyncResult
	for token, _ := range this.CloneToken2ChannelGraph() {
		mgr, _ := this.ConnectionManagerForToken(token)
		if mgr != nil {
			leaveResults = append(leaveResults, mgr.LeaveAsync())
		}
	}
	return WaitGroupAsyncResult(leaveResults)
}
func WaitGroupAsyncResult(results []*network.AsyncResult) *network.AsyncResult {
	totalResult := network.NewAsyncResult()
	wg := sync.WaitGroup{}
	wg.Add(len(results))
	for i, _ := range results {
		go func(i int) {
			<-results[i].Result
			wg.Done()
		}(i)
	}
	go func() {
		wg.Wait()
		totalResult.Result <- nil
		close(totalResult.Result)
	}()
	return totalResult
}
func (this *RaidenService) CloseAndSettle() {
	log.Info("raiden will close and settle all channels now")
	var Mgrs []*ConnectionManager
	for t, _ := range this.Token2ChannelGraph {
		mgr, _ := this.ConnectionManagerForToken(t)
		if mgr != nil {
			Mgrs = append(Mgrs, mgr)
		}
	}
	blocksToWait := func() int64 {
		var max int64 = 0
		for _, mgr := range Mgrs {
			if max < mgr.minSettleBlocks() {
				max = mgr.minSettleBlocks()
			}
		}
		return max
	}
	var AllChannels []*channel.Channel
	for _, mgr := range Mgrs {
		for _, c := range mgr.openChannels() {
			ch, err := this.FindChannelByAddress(c.ChannelAddress)
			if err != nil {
				panic(fmt.Sprintf("channel %s must exist", utils.APex(c.ChannelAddress)))
			}
			AllChannels = append(AllChannels, ch)
		}
	}
	leavingResult := this.LeaveAllTokenNetworksAsync()
	//using the un-cached block number here
	lastBlock := this.GetBlockNumber()
	earliestSettlement := lastBlock + blocksToWait()
	/*
			    TODO: estimate and set a `timeout` parameter in seconds
		     based on connection_manager.min_settle_blocks and an average
		     blocktime from the past
	*/
	currentBlock := lastBlock
	for currentBlock < earliestSettlement {
		time.Sleep(time.Second * 10)
		lastBlock := this.GetBlockNumber()
		if lastBlock != currentBlock {
			currentBlock = lastBlock
			waitBlocksLeft := blocksToWait()
			notSettled := 0
			for _, c := range AllChannels {
				if c.State() != transfer.ChannelStateSettled {
					notSettled++
				}
			}
			if notSettled == 0 {
				log.Debug("nothing left to settle")
				break
			}
			log.Info(fmt.Sprintf("waiting at least %s more blocks for %d channels not yet settled", waitBlocksLeft, notSettled))
		}
		//why  leaving_greenlet.wait
		timeoutch := time.After(time.Second * time.Duration(blocksToWait()))
		select {
		case <-timeoutch:
		case <-leavingResult.Result:
		}
	}
	for _, c := range AllChannels {
		if c.State() != transfer.ChannelStateSettled {
			log.Error("channels were not settled:", utils.APex(c.MyAddress))
		}
	}
}

/*
Do a direct tranfer with target.

       Direct transfers are non cancellable and non expirable, since these
       transfers are a signed balance proof with the transferred amount
       incremented.

       Because the transfer is non cancellable, there is a level of trust with
       the target. After the message is sent the target is effectively paid
       and then it is not possible to revert.

       The async result will be set to False iff there is no direct channel
       with the target or the payer does not have balance to complete the
       transfer, otherwise because the transfer is non expirable the async
       result *will never be set to False* and if the message is sent it will
       hang until the target node acknowledge the message.

       This transfer should be used as an optimization, since only two packets
       are required to complete the transfer (from the payer's perspective),
       whereas the mediated transfer requires 6 messages.
*/
func (this *RaidenService) DirectTransferAsync(tokenAddress, target common.Address, amount *big.Int, identifier uint64) (result *network.AsyncResult) {
	graph := this.GetToken2ChannelGraph(tokenAddress)
	directChannel := graph.GetPartenerAddress2Channel(target)
	result = network.NewAsyncResult()
	if directChannel == nil || !directChannel.CanTransfer() || directChannel.Distributable().Cmp(amount) < 0 {
		result.Result <- errors.New("no available direct channel")
		return
	}
	tr, err := directChannel.CreateDirectTransfer(amount, identifier)
	if err != nil {
		result.Result <- err
		return
	}
	tr.Sign(this.PrivateKey, tr)
	directChannel.RegisterTransfer(this.GetBlockNumber(), tr)
	directTransferStateChange := &transfer.ActionTransferDirectStateChange{
		Identifier:   identifier,
		Amount:       amount,
		TokenAddress: tokenAddress,
		NodeAddress:  directChannel.PartnerState.Address,
	}
	// TODO: add the transfer sent event
	stateChangeId, _ := this.db.LogStateChange(directTransferStateChange)
	//This should be set once the direct transfer is acknowledged
	transferSuccess := transfer.EventTransferSentSuccess{
		Identifier: identifier,
		Amount:     amount,
		Target:     target,
	}
	this.db.LogEvents(stateChangeId, []transfer.Event{transferSuccess}, this.GetBlockNumber())
	result = this.Protocol.SendAsync(directChannel.PartnerState.Address, tr)
	return
}

/*
mediated transfer for token swap
we must make sure that taker use the maker's secret.
and taker's lock expiration should be short than maker's todo(fix this)
*/
func (this *RaidenService) StartTakerMediatedTransfer(tokenAddress, target common.Address, amount *big.Int, identifier uint64, hashlock common.Hash, expiration int64) (result *network.AsyncResult, stateManager *transfer.StateManager) {
	return this.startMediatedTransferInternal(tokenAddress, target, amount, utils.BigInt0, identifier, hashlock, expiration)
}

/*
lauch a new mediated trasfer
Args:
 hashlock: caller can specify a hashlock or use empty ,when empty, will generate a random secret.
 expiration: caller can specify a valid blocknumber or 0, when 0 ,will calculate based on settle timeout of channel.
*/
func (this *RaidenService) startMediatedTransferInternal(tokenAddress, target common.Address, amount *big.Int, fee *big.Int, identifier uint64, hashlock common.Hash, expiration int64) (result *network.AsyncResult, stateManager *transfer.StateManager) {
	graph := this.GetToken2ChannelGraph(tokenAddress)
	availableRoutes := graph.GetBestRoutes(this.Protocol, this.NodeAddress, target, amount, utils.EmptyAddress, this)
	result = network.NewAsyncResult()
	result.Tag = target //tell the difference when token swap
	if len(availableRoutes) <= 0 {
		result.Result <- errors.New("no available route")
		return
	}
	if identifier == 0 {
		identifier = rand.New(utils.RandSrc).Uint64()
	}
	/*
		when user specified fee, for test or other purpose.
	*/
	if fee.Cmp(utils.BigInt0) > 0 {
		for _, r := range availableRoutes {
			r.TotalFee = fee //use the user's fee to replace algorithm's
		}
	}
	routesState := transfer.NewRoutesState(availableRoutes)
	transferState := &mediated_transfer.LockedTransferState{
		Identifier:   identifier,
		TargetAmount: new(big.Int).Set(amount),
		Amount:       new(big.Int).Set(amount),
		Token:        tokenAddress,
		Initiator:    this.NodeAddress,
		Target:       target,
		Expiration:   expiration,
		Hashlock:     utils.EmptyHash,
		Secret:       utils.EmptyHash,
		Fee:          utils.BigInt0,
	}
	/*
			  Issue #489

		        Raiden may fail after a state change using the random generator is
		        handled but right before the snapshot is taken. If that happens on
		        the next initialization when raiden is recovering and applying the
		        pending state changes a new secret will be generated and the
		        resulting events won't match, this breaks the architecture model,
		        since it's assumed the re-execution of a state change will always
		        produce the same events.

		        TODO: Removed the secret generator from the InitiatorState and add
		        the secret into all state changes that require one, this way the
		        secret will be serialized with the state change and the recovery will
		        use the same /random/ secret.
	*/
	initInitiator := &mediated_transfer.ActionInitInitiatorStateChange{
		OurAddress:  this.NodeAddress,
		Tranfer:     transferState,
		Routes:      routesState,
		BlockNumber: this.GetBlockNumber(),
		Db:          this.db,
	}
	if hashlock == utils.EmptyHash {
		initInitiator.RandomGenerator = utils.RandomSecretGenerator
	} else {
		initInitiator.RandomGenerator = utils.NewSepecifiedSecretGenerator(hashlock)
	}
	stateManager = transfer.NewStateManager(initiator.StateTransition, nil, initiator.NameInitiatorTransition, transferState.Identifier, transferState.Token)
	/*
		  TODO: implement the network timeout raiden.config['msg_timeout'] and
			cancel the current transfer if it hapens (issue #374)
	*/
	/*
		first register the this transfer id, otherwise error may occur imediatelly
	*/
	mgrs := this.Identifier2StateManagers[identifier]
	mgrs = append(mgrs, stateManager)
	this.Identifier2StateManagers[identifier] = mgrs
	results := this.Identifier2Results[identifier]
	results = append(results, result)
	this.Identifier2Results[identifier] = results
	this.db.AddStateManager(stateManager)
	//ping before send transfer
	this.RoutesTask.NewTask <- &RoutesToDetect{
		RoutesState:     initInitiator.Routes,
		StateManager:    stateManager,
		InitStateChange: initInitiator,
	}
	//this.StateMachineEventHandler.LogAndDispatch(stateManager, initInitiator)
	return
}

/*
1. user start a mediated transfer
2. user start a maker mediated transfer
*/
func (this *RaidenService) StartMediatedTransfer(tokenAddress, target common.Address, amount *big.Int, fee *big.Int, identifier uint64) (result *network.AsyncResult) {
	result, _ = this.startMediatedTransferInternal(tokenAddress, target, amount, fee, identifier, utils.EmptyHash, 0)
	return
}

//receive a MediatedTransfer, i'm a hop node
func (this *RaidenService) MediateMediatedTransfer(msg *encoding.MediatedTransfer) {
	amount := msg.Amount
	target := msg.Target
	token := msg.Token
	graph := this.GetToken2ChannelGraph(token)
	avaiableRoutes := graph.GetBestRoutes(this.Protocol, this.NodeAddress, target, amount, msg.Sender, this)
	fromChannel := graph.GetPartenerAddress2Channel(msg.Sender)
	fromRoute := network.Channel2RouteState(fromChannel, msg.Sender, amount, this)
	ourAddress := this.NodeAddress
	fromTransfer := mediated_transfer.LockedTransferFromMessage(msg)
	routesState := transfer.NewRoutesState(avaiableRoutes)
	blockNumber := this.GetBlockNumber()
	initMediator := &mediated_transfer.ActionInitMediatorStateChange{
		OurAddress:  ourAddress,
		FromTranfer: fromTransfer,
		Routes:      routesState,
		FromRoute:   fromRoute,
		BlockNumber: blockNumber,
		Message:     msg,
		Db:          this.db,
	}
	stateManager := transfer.NewStateManager(mediator.StateTransition, nil, mediator.NameMediatorTransition, fromTransfer.Identifier, fromTransfer.Token)
	this.db.AddStateManager(stateManager)
	mgrs := this.Identifier2StateManagers[msg.Identifier]
	mgrs = append(mgrs, stateManager)
	this.Identifier2StateManagers[msg.Identifier] = mgrs //for path A-B-C-F-B-D-E ,node B will have two StateManagers for one identifier
	//ping before send transfer
	this.RoutesTask.NewTask <- &RoutesToDetect{
		RoutesState:     initMediator.Routes,
		StateManager:    stateManager,
		InitStateChange: initMediator,
	}
	//this.StateMachineEventHandler.LogAndDispatch(stateManager, initMediator)
}

//receive a MediatedTransfer, i'm the target
func (this *RaidenService) TargetMediatedTransfer(msg *encoding.MediatedTransfer) {
	graph := this.GetToken2ChannelGraph(msg.Token)
	fromChannel := graph.GetPartenerAddress2Channel(msg.Sender)
	fromRoute := network.Channel2RouteState(fromChannel, msg.Sender, msg.Amount, this)
	fromTransfer := mediated_transfer.LockedTransferFromMessage(msg)
	initTarget := &mediated_transfer.ActionInitTargetStateChange{
		OurAddress:  this.NodeAddress,
		FromRoute:   fromRoute,
		FromTranfer: fromTransfer,
		BlockNumber: this.GetBlockNumber(),
		Message:     msg,
		Db:          this.db,
	}
	stateManger := transfer.NewStateManager(target.StateTransiton, nil, target.NameTargetTransition, fromTransfer.Identifier, fromTransfer.Token)
	this.db.AddStateManager(stateManger)
	identifier := msg.Identifier
	mgrs := this.Identifier2StateManagers[identifier]
	mgrs = append(mgrs, stateManger)
	this.Identifier2StateManagers[identifier] = mgrs

	this.StateMachineEventHandler.LogAndDispatch(stateManger, initTarget)
}

func (this *RaidenService) StartHealthCheckFor(address common.Address) {
	if !this.Config.EnableHealthCheck {
		return
	}
	if this.HealthCheckMap[address] {
		log.Info(fmt.Sprintf("addr %s check already start.", utils.APex(address)))
		return
	}
	this.HealthCheckMap[address] = true
	go func() {
		log.Trace(fmt.Sprintf("health check for %s started", utils.APex(address)))
		for {
			err := this.Protocol.SendPing(address)
			if err != nil {
				log.Info("health check ping %s err %s", utils.APex(address), err)
			}
			time.Sleep(time.Second * 10)
		}
	}()
}

func (this *RaidenService) StartNeighboursHealthCheck() {
	for _, g := range this.Token2ChannelGraph {
		for addr := range g.PartenerAddress2Channel {
			this.StartHealthCheckFor(addr)
		}
	}
}

func (this *RaidenService) GetToken2ChannelGraph(tokenAddress common.Address) (cg *network.ChannelGraph) {
	cg = this.Token2ChannelGraph[tokenAddress]
	if cg == nil {
		log.Error(fmt.Sprintf("%s token doesn't exist ", utils.APex(tokenAddress)))
	}
	return
}

/*
todo remove this function, there is no need lock .
*/
func (this *RaidenService) CloneToken2ChannelGraph() map[common.Address]*network.ChannelGraph {
	return this.Token2ChannelGraph
	//m := make(map[common.Address]*network.ChannelGraph)
	//this.Lock.RLock()
	//defer this.Lock.RUnlock()
	//for k, v := range this.Token2ChannelGraph {
	//	m[k] = v
	//}
	//return m
}

//only for test, should call FindChannelByAddress
func (this *RaidenService) GetChannelWithAddr(channelAddr common.Address) *channel.Channel {
	c, _ := this.FindChannelByAddress(channelAddr)
	return c
}

//for test
func (this *RaidenService) getChannel(tokenAddr, partnerAddr common.Address) *channel.Channel {
	g := this.GetToken2ChannelGraph(tokenAddr)
	if g == nil {
		return nil
	}
	return g.GetPartenerAddress2Channel(partnerAddr)
}

/*
Process user's new channel request
*/
func (this *RaidenService) newChannel(token, partner common.Address, settleTimeout int) (result *network.AsyncResult) {
	result = network.NewAsyncResult()
	go func() {
		var err error
		defer func() {
			result.Result <- err
			close(result.Result)
		}()
		chMgrAddr, err := this.Registry.ChannelManagerByToken(token)
		if err != nil {
			return
		}
		chMgr := this.Chain.Manager(chMgrAddr)
		_, err = chMgr.NewChannel(partner, settleTimeout)
		if err != nil {
			return
		}
		//defer write result
	}()
	return
}

/*
process user's deposit request
*/
func (this *RaidenService) depositChannel(channelAddress common.Address, amount *big.Int) (result *network.AsyncResult) {
	result = network.NewAsyncResult()
	c, err := this.FindChannelByAddress(channelAddress)
	if err != nil {
		result.Result <- err
		return
	}
	go func() {
		err := c.ExternState.Deposit(amount)
		result.Result <- err
		close(result.Result)
	}()
	return
}

/*
process user's close or settle channel request
*/
func (this *RaidenService) closeOrSettleChannel(channelAddress common.Address, op string) (result *network.AsyncResult) {
	result = network.NewAsyncResult()
	c, err := this.FindChannelByAddress(channelAddress)
	if err != nil { //settled channel can be queried from db.
		result.Result <- errors.New("channel not exist")
		return
	}
	log.Trace(fmt.Sprintf("%s channel %s\n", op, utils.APex(channelAddress)))
	go func() {
		var err error
		c2, _ := this.db.GetChannelByAddress(c.MyAddress)
		proof := c2.PartnerBalanceProof
		if op == CloseChannelReqName {
			err = c.ExternState.Close(proof)
		} else {
			err = c.ExternState.Settle()
		}
		log.Trace(fmt.Sprintf("%s channel finished err %v", op, err))
		result.Result <- err
	}()
	return
}

/*
process user's token swap maker request
save and restore todo?
*/
func (this *RaidenService) tokenSwapMaker(tokenswap *TokenSwap) (result *network.AsyncResult) {
	var hashlock common.Hash
	var hasReceiveTakerMediatedTransfer bool
	var sentMtrHook SentMediatedTransferListener
	var receiveMtrHook ReceivedMediatedTrasnferListener
	var secretRequestHook SecretRequestPredictor
	secretRequestHook = func(msg *encoding.SecretRequest) (ignore bool) {
		if !hasReceiveTakerMediatedTransfer {
			/*
				ignore secret request until recieve a valid taker mediated transfer.
				we assume that :
				taker have two independent queue for secret request and mediated transfer
			*/
			return true
		}
		delete(this.SecretRequestPredictorMap, hashlock) //old hashlock is invalid,just  remove
		return false
	}
	sentMtrHook = func(mtr *encoding.MediatedTransfer) (remove bool) {
		if mtr.Identifier == tokenswap.Identifier && mtr.Token == tokenswap.FromToken && mtr.Target == tokenswap.ToNodeAddress && mtr.Amount.Cmp(tokenswap.FromAmount) == 0 {
			if hashlock != utils.EmptyHash {
				log.Info(fmt.Sprintf("tokenswap maker select new path ,because of different hash lock"))
				delete(this.SecretRequestPredictorMap, hashlock) //old hashlock is invalid,just  remove
			}
			hashlock = mtr.HashLock //hashlock may change when select new route path
			this.SecretRequestPredictorMap[hashlock] = secretRequestHook
		}
		return false
	}
	receiveMtrHook = func(mtr *encoding.MediatedTransfer) (remove bool) {
		/*
			recevive taker's mediated transfer , the transfer must use argument of tokenswap and have the same hashlock
		*/
		if mtr.Identifier == tokenswap.Identifier && hashlock == mtr.HashLock && mtr.Token == tokenswap.ToToken && mtr.Target == tokenswap.FromNodeAddress && mtr.Amount.Cmp(tokenswap.ToAmount) == 0 {
			hasReceiveTakerMediatedTransfer = true
			delete(this.SentMediatedTransferListenerMap, &sentMtrHook)
			return true
		}
		return false
	}
	this.SentMediatedTransferListenerMap[&sentMtrHook] = true
	this.ReceivedMediatedTrasnferListenerMap[&receiveMtrHook] = true
	result = this.StartMediatedTransfer(tokenswap.FromToken, tokenswap.ToNodeAddress, tokenswap.FromAmount, utils.BigInt0, tokenswap.Identifier)
	return
}

/*
taker process token swap
taker's action is triggered by maker's mediated transfer.
*/
func (this *RaidenService) messageTokenSwapTaker(msg *encoding.MediatedTransfer, tokenswap *TokenSwap) (remove bool) {
	var hashlock common.Hash = msg.HashLock
	var hasReceiveRevealSecret bool
	var stateManager *transfer.StateManager
	if msg.Identifier != tokenswap.Identifier || msg.Amount.Cmp(tokenswap.FromAmount) != 0 || msg.Initiator != tokenswap.FromNodeAddress || msg.Token != tokenswap.FromToken || msg.Target != tokenswap.ToNodeAddress {
		log.Info("receive a mediated transfer, not match tokenswap condition")
		return false
	}
	log.Trace(fmt.Sprintf("begin token swap for %s", msg))
	var secretRequestHook SecretRequestPredictor = func(msg *encoding.SecretRequest) (ignore bool) {
		if !hasReceiveRevealSecret {
			/*
				ignore secret request until recieve a valid reveal secret.
				we assume that :
				maker first send a valid reveal secret and then send secret request, otherwis may deadlock but  taker willnot lose tokens.
			*/
			return true
		}
		return false
	}
	var receiveRevealSecretHook RevealSecretListener = func(msg *encoding.RevealSecret) (remove bool) {
		if msg.HashLock() != hashlock {
			return false
		}
		state := stateManager.CurrentState
		initState, ok := state.(*mediated_transfer.InitiatorState)
		if !ok {
			panic(fmt.Sprintf("must be a InitiatorState"))
		}
		if initState.Transfer.Hashlock != msg.HashLock() {
			panic(fmt.Sprintf("hashlock must be same , state lock=%s,msg lock=%s", utils.HPex(initState.Transfer.Hashlock), utils.HPex(msg.HashLock())))
		}
		initState.Transfer.Secret = msg.Secret
		hasReceiveRevealSecret = true
		delete(this.SecretRequestPredictorMap, hashlock)
		return true
	}
	/*
		taker's Expiration must be smaller than maker's ,
		taker and maker may have direct channels on these two tokens.
	*/
	takerExpiration := msg.Expiration - params.DefaultRevealTimeout
	result, stateManager := this.StartTakerMediatedTransfer(tokenswap.ToToken, tokenswap.FromNodeAddress, tokenswap.ToAmount, tokenswap.Identifier, msg.HashLock, takerExpiration)
	if stateManager == nil {
		log.Error(fmt.Sprintf("taker tokenwap error %s", <-result.Result))
		return false
	}
	this.SecretRequestPredictorMap[hashlock] = secretRequestHook
	this.RevealSecretListenerMap[hashlock] = receiveRevealSecretHook
	return true
}

/*
process taker's token swap
only mark, if i receive a valid mediated transfer, then start token swap
*/
func (this *RaidenService) tokenSwapTaker(tokenswap *TokenSwap) (result *network.AsyncResult) {
	result = network.NewAsyncResult()
	result.Result <- nil
	key := SwapKey{
		Identifier: tokenswap.Identifier,
		FromToken:  tokenswap.FromToken,
		FromAmount: tokenswap.FromAmount.String(),
	}
	this.SwapKey2TokenSwap[key] = tokenswap
	return
}

//all user's request
func (this *RaidenService) handleReq(req *ApiReq) {
	var result *network.AsyncResult
	switch req.Name {
	case TransferReqName: //mediated transfer only
		r := req.Req.(*TransferReq)
		result = this.StartMediatedTransfer(r.TokenAddress, r.Target, r.Amount, r.Fee, r.Identifier)
	case NewChannelReqName:
		r := req.Req.(*NewChannelReq)
		result = this.newChannel(r.tokenAddress, r.partnerAddress, r.settleTimeout)
	case DepositChannelReqName:
		r := req.Req.(*DepositChannelReq)
		result = this.depositChannel(r.addr, r.amount)
	case CloseChannelReqName:
		r := req.Req.(*CloseSettleChannelReq)
		result = this.closeOrSettleChannel(r.addr, req.Name)
	case SettleChannelReqName:
		r := req.Req.(*CloseSettleChannelReq)
		result = this.closeOrSettleChannel(r.addr, req.Name)
	case TokenSwapMakerReqName:
		r := req.Req.(*TokenSwapMakerReq)
		result = this.tokenSwapMaker(r.tokenSwap)
	case TokenSwapTakerReqName:
		r := req.Req.(*TokenSwapTakerReq)
		result = this.tokenSwapTaker(r.tokenSwap)
	default:
		panic("unkown req")
	}
	r := req
	r.result <- result
}

//recieve a ack from
func (this *RaidenService) handleSentMessage(sentMessage *ProtocolMessage) {
	log.Trace(fmt.Sprintf("msg receive ack :%s", utils.StringInterface(sentMessage, 2)))
	if sentMessage.Message.Tag() != nil { //
		sentMessageTag := sentMessage.Message.Tag().(*transfer.MessageTag)
		if sentMessageTag.GetStateManager() != nil {
			mgr := sentMessageTag.GetStateManager()
			mgr.ManagerState = transfer.StateManagerSendMessageSuccesss
			sentMessageTag.SendingMessageComplete = true
			tx := this.db.StartTx()
			_, ok := sentMessage.Message.(*encoding.Secret)
			if ok {
				mgr.IsBalanceProofSent = true
				if mgr.Name == initiator.NameInitiatorTransition {
					mgr.ManagerState = transfer.StateManagerTransferComplete
				} else if mgr.Name == target.NameTargetTransition {

				} else if mgr.Name == mediator.NameMediatorTransition {
					/*
						how to detect a mediator node is finish or not?
							1. receive prev balanceproof
							2. balanceproof  send to next successfully
						//todo when refund?
					*/
					if mgr.IsBalanceProofSent && mgr.IsBalanceProofReceived {
						mgr.ManagerState = transfer.StateManagerTransferComplete
					}

				}
			}
			this.db.UpdateStateManaer(mgr, tx)
			tx.Commit()
			this.ConditionQuit(fmt.Sprintf("%sRecevieAck", sentMessage.Message.Name()))
		} else if sentMessageTag.EchoHash != utils.EmptyHash {
			//log.Trace(fmt.Sprintf("reveal sent complete %s", utils.StringInterface(sentMessage.Message, 5)))
			this.ConditionQuit(fmt.Sprintf("%sRecevieAck", sentMessage.Message.Name()))
			switch msg := sentMessage.Message.(type) {
			case *encoding.RevealSecret:
				this.db.UpdateSentRevealSecretComplete(sentMessageTag.EchoHash)
			case *encoding.RemoveExpiredHashlockTransfer:
				this.db.UpdateSentRemoveExpiredHashlockTransfer(sentMessageTag.EchoHash)
			default:
				log.Error(fmt.Sprintf("unknown message %s", utils.StringInterface(msg, 7)))
			}

		} else {
			panic(fmt.Sprintf("sent message state unknow :%s", utils.StringInterface(sentMessageTag, 2)))
		}
	} else {
		log.Error(fmt.Sprintf("message must have tag, only when make token swap %s", utils.StringInterface(sentMessage.Message, 3)))
	}
}
func (this *RaidenService) handleRoutesTask(task *RoutesToDetect) {
	/*
		no need to modify InitStateChange's Routes, because RoutesTask share the same instance
	*/
	switch task.InitStateChange.(type) {
	case *mediated_transfer.ActionInitInitiatorStateChange:
		//do nothing
	case *mediated_transfer.ActionInitMediatorStateChange:
		//do nothing
	}
	this.StateMachineEventHandler.LogAndDispatch(task.StateManager, task.InitStateChange)
}

/*
implement of FeeCharger
*/
func (this *RaidenService) GetNodeChargeFee(nodeAddress, tokenAddress common.Address, amount *big.Int) *big.Int {
	return this.FeePolicy.GetNodeChargeFee(nodeAddress, tokenAddress, amount)
}
func (this *RaidenService) SetFeePolicy(feePolicy fee.FeeCharger) {
	this.FeePolicy = feePolicy
}

/*
for debug only,quit if eventName exactly match
*/
func (this *RaidenService) ConditionQuit(eventName string) {
	if strings.ToLower(eventName) == strings.ToLower(this.Config.ConditionQuit.QuitEvent) {
		log.Error(fmt.Sprintf("quitevent=%s\n", eventName))
		debug.PrintStack()
		os.Exit(111)
	}
}
