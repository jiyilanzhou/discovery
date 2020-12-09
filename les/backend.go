// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package les implements the Light Truechain Subprotocol.
package les

import (
	"fmt"
	"sync"
	"time"
	"truechain/discovery/accounts/abi/bind"
	"truechain/discovery/common/mclock"
	"truechain/discovery/light/fast"
	"truechain/discovery/light/public"

	"truechain/discovery/accounts"
	"truechain/discovery/common"
	"truechain/discovery/common/hexutil"
	"truechain/discovery/consensus"
	"truechain/discovery/core"
	"truechain/discovery/core/bloombits"
	"truechain/discovery/core/rawdb"
	"truechain/discovery/core/types"
	"truechain/discovery/etrue"
	"truechain/discovery/etrue/downloader"
	"truechain/discovery/etrue/filters"
	"truechain/discovery/etrue/gasprice"
	"truechain/discovery/event"
	"truechain/discovery/internal/trueapi"
	"truechain/discovery/light"
	"truechain/discovery/log"
	"truechain/discovery/node"
	"truechain/discovery/p2p"
	"truechain/discovery/p2p/discv5"
	"truechain/discovery/params"
	"truechain/discovery/rpc"
)

type LightEtrue struct {
	lesCommons

	odr         *LesOdr
	chainConfig *params.ChainConfig
	// Channel for shutting down the service
	shutdownChan chan bool

	// Handlers
	peers       *peerSet
	txPool      *fast.TxPool
	election    *Election
	blockchain  *light.LightChain
	fblockchain *fast.LightChain
	serverPool  *serverPool
	reqDist     *requestDistributor
	retriever   *retrieveManager
	relay       *lesTxRelay

	bloomRequests chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer  *core.ChainIndexer

	ApiBackend *LesApiBackend

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager

	networkId     uint64
	netRPCService *trueapi.PublicNetAPI

	wg sync.WaitGroup
}

func New(ctx *node.ServiceContext, config *etrue.Config) (*LightEtrue, error) {
	chainDb, err := etrue.CreateDB(ctx, config, "lightchaindata")
	if err != nil {
		return nil, err
	}
	chainConfig, genesisHash, snailGenesis, genesisErr := core.SetupGenesisBlock(chainDb, config.Genesis)
	if _, isCompat := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !isCompat {
		return nil, genesisErr
	}
	log.Info("Initialised chain configuration", "config", chainConfig)

	peers := newPeerSet()
	quitSync := make(chan struct{})

	leth := &LightEtrue{
		lesCommons: lesCommons{
			chainDb: chainDb,
			config:  config,
			iConfig: public.DefaultClientIndexerConfig,
		},
		chainConfig:    chainConfig,
		eventMux:       ctx.EventMux,
		peers:          peers,
		reqDist:        newRequestDistributor(peers, quitSync, &mclock.System{}),
		accountManager: ctx.AccountManager,
		engine:         etrue.CreateConsensusEngine(ctx, &config.MinervaHash, chainConfig, chainDb),
		shutdownChan:   make(chan bool),
		networkId:      config.NetworkId,
		bloomRequests:  make(chan chan *bloombits.Retrieval),
		//bloomIndexer:   etrue.NewBloomIndexer(chainDb, params.BloomBitsBlocksClient, params.HelperTrieConfirmations),
	}

	leth.serverPool = newServerPool(chainDb, quitSync, &leth.wg, nil)
	leth.retriever = newRetrieveManager(peers, leth.reqDist, leth.serverPool)
	leth.relay = newLesTxRelay(peers, leth.retriever)

	leth.odr = NewLesOdr(chainDb, public.DefaultClientIndexerConfig, leth.retriever)
	leth.chtIndexer = light.NewChtIndexer(chainDb, leth.odr, params.CHTFrequency, params.HelperTrieConfirmations)
	leth.bloomTrieIndexer = fast.NewBloomTrieIndexer(chainDb, leth.odr, params.BloomBitsBlocksClient, params.BloomTrieFrequency)
	leth.odr.SetIndexers(leth.chtIndexer, leth.bloomTrieIndexer, leth.bloomIndexer)

	checkpoint := params.TrustedCheckpoints[snailGenesis]

	if leth.fblockchain, err = fast.NewLightChain(leth.odr, leth.chainConfig, leth.engine, checkpoint); err != nil {
		return nil, err
	}
	// Note: NewLightChain adds the trusted checkpoint so it needs an ODR with
	// indexers already set but not started yet
	if leth.blockchain, err = light.NewLightChain(leth.fblockchain, leth.odr, leth.chainConfig, leth.engine, checkpoint); err != nil {
		return nil, err
	}
	leth.election = NewLightElection(leth.fblockchain, leth.blockchain)
	leth.engine.SetElection(leth.election)
	leth.engine.SetSnailChainReader(leth.blockchain.GetHeaderChain())
	leth.engine.SetSnailHeaderHash(chainDb)

	// Note: AddChildIndexer starts the update process for the child
	leth.chtIndexer.Start(leth.blockchain)
	//leth.bloomIndexer.AddChildIndexer(leth.bloomTrieIndexer)
	//leth.bloomIndexer.Start(leth.fblockchain)

	// Rewind the chain in case of an incompatible config upgrade.
	if compat, ok := genesisErr.(*params.ConfigCompatError); ok {
		log.Warn("Rewinding chain to upgrade configuration", "err", compat)
		leth.blockchain.SetHead(compat.RewindTo)
		rawdb.WriteChainConfig(chainDb, genesisHash, chainConfig)
	}

	leth.txPool = fast.NewTxPool(leth.chainConfig, leth.fblockchain, leth.relay)
	leth.ApiBackend = &LesApiBackend{false, leth, nil}
	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.GasPrice
	}
	leth.ApiBackend.gpo = gasprice.NewOracle(leth.ApiBackend, gpoParams)

	if leth.protocolManager, err = NewProtocolManager(leth.chainConfig, checkpoint, public.DefaultClientIndexerConfig, nil, 0, true, config.NetworkId, leth.eventMux, leth.engine, leth.peers, leth.fblockchain, leth.blockchain, nil, chainDb, leth.odr, leth.serverPool, nil, quitSync, &leth.wg, leth.election, nil); err != nil {
		return nil, err
	}
	if leth.protocolManager.ulc != nil {
		log.Warn("Ultra light client is enabled")
		leth.blockchain.DisableCheckFreq()
	}
	return leth, nil
}

func lesTopic(genesisHash common.Hash, protocolVersion uint) discv5.Topic {
	var name string
	switch protocolVersion {
	case lpv2:
		name = "LES2"
	default:
		panic(nil)
	}
	return discv5.Topic(name + "@" + common.Bytes2Hex(genesisHash.Bytes()[0:8]))
}

type LightDummyAPI struct{}

// Etherbase is the address that mining rewards will be send to
func (s *LightDummyAPI) Etherbase() (common.Address, error) {
	return common.Address{}, fmt.Errorf("mining is not supported in light mode")
}

// Coinbase is the address that mining rewards will be send to (alias for Etherbase)
func (s *LightDummyAPI) Coinbase() (common.Address, error) {
	return common.Address{}, fmt.Errorf("mining is not supported in light mode")
}

// Hashrate returns the POW hashrate
func (s *LightDummyAPI) Hashrate() hexutil.Uint {
	return 0
}

// Mining returns an indication if this node is currently mining.
func (s *LightDummyAPI) Mining() bool {
	return false
}

// APIs returns the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *LightEtrue) APIs() []rpc.API {
	apis := trueapi.GetAPIs(s.ApiBackend)
	namespaces := []string{"etrue", "eth"}
	for _, name := range namespaces {
		apis = append(apis, []rpc.API{
			{
				Namespace: name,
				Version:   "1.0",
				Service:   &LightDummyAPI{},
				Public:    true,
			}, {
				Namespace: name,
				Version:   "1.0",
				Service:   downloader.NewPublicDownloaderAPI(s.protocolManager.downloader, s.eventMux),
				Public:    true,
			}, {
				Namespace: name,
				Version:   "1.0",
				Service:   filters.NewPublicFilterAPI(s.ApiBackend, true),
				Public:    true,
			},
		}...)
	}
	apis = append(apis, []rpc.API{
		{
			Namespace: "net",
			Version:   "1.0",
			Service:   s.netRPCService,
			Public:    true,
		}, {
			Namespace: "les",
			Version:   "1.0",
			Service:   NewPrivateLightAPI(&s.lesCommons, s.protocolManager.reg),
			Public:    false,
		},
	}...)
	return apis
}

func (s *LightEtrue) ResetWithGenesisBlock(gb *types.Block) {
	s.fblockchain.ResetWithGenesisBlock(gb)
}

func (s *LightEtrue) SnailBlockChain() *light.LightChain { return s.blockchain }
func (s *LightEtrue) BlockChain() *fast.LightChain       { return s.fblockchain }
func (s *LightEtrue) TxPool() *fast.TxPool               { return s.txPool }
func (s *LightEtrue) Engine() consensus.Engine           { return s.engine }
func (s *LightEtrue) LesVersion() int                    { return int(ClientProtocolVersions[0]) }
func (s *LightEtrue) Downloader() *downloader.Downloader { return s.protocolManager.downloader }
func (s *LightEtrue) EventMux() *event.TypeMux           { return s.eventMux }

// Protocols implements node.Service, returning all the currently configured
// network protocols to start.
func (s *LightEtrue) Protocols() []p2p.Protocol {
	return s.makeProtocols(ClientProtocolVersions)
}

// Start implements node.Service, starting all internal goroutines needed by the
// Truechain protocol implementation.
func (s *LightEtrue) Start(srvr *p2p.Server) error {
	log.Warn("Light client mode is an experimental feature")
	s.startBloomHandlers(params.BloomBitsBlocksClient)
	s.netRPCService = trueapi.NewPublicNetAPI(srvr, s.networkId)
	// clients are searching for the first advertised protocol in the list
	protocolVersion := AdvertiseProtocolVersions[0]
	s.serverPool.start(srvr, lesTopic(s.SnailBlockChain().Genesis().Hash(), protocolVersion))
	s.protocolManager.Start(s.config.LightPeers)
	return nil
}

// Stop implements node.Service, terminating all internal goroutines used by the
// Truechain protocol.
func (s *LightEtrue) Stop() error {
	s.odr.Stop()
	s.relay.Stop()
	//s.bloomIndexer.Close()
	s.chtIndexer.Close()
	s.blockchain.Stop()
	s.fblockchain.Stop()
	s.protocolManager.Stop()
	s.txPool.Stop()
	//s.engine.Close()

	s.eventMux.Stop()

	time.Sleep(time.Millisecond * 200)
	s.chainDb.Close()
	close(s.shutdownChan)

	return nil
}

// SetClient sets the rpc client and binds the registrar contract.
func (s *LightEtrue) SetContractBackend(backend bind.ContractBackend) {
	// Short circuit if registrar is nil
	if s.protocolManager.reg == nil {
		return
	}
	s.protocolManager.reg.start(backend)
}
