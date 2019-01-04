package miningpool

import (
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/account"
	"github.com/bytom/event"
	"github.com/bytom/mining"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc/types"
)

const (
	maxSubmitChSize = 50
)

// TODO:
// 1. adjust recomit interval
// 2. custom recomit interval
var recommitTicker = time.NewTicker(3 * time.Second) // RecommitInterval for eth lies in [1s, 15s]

type submitBlockMsg struct {
	blockHeader *types.BlockHeader
	reply       chan error
}

// MiningPool is the support struct for p2p mine pool
type MiningPool struct {
	mutex     sync.RWMutex
	block     *types.Block
	submitCh  chan *submitBlockMsg
	commitMap map[types.BlockCommitment]([]*types.Tx)

	chain           *protocol.Chain
	accountManager  *account.Manager
	txPool          *protocol.TxPool
	eventDispatcher *event.Dispatcher
}

// NewMiningPool will create a new MiningPool
func NewMiningPool(c *protocol.Chain, accountManager *account.Manager, txPool *protocol.TxPool, dispatcher *event.Dispatcher) *MiningPool {
	m := &MiningPool{
		submitCh:        make(chan *submitBlockMsg, maxSubmitChSize),
		commitMap:       make(map[types.BlockCommitment]([]*types.Tx)),
		chain:           c,
		accountManager:  accountManager,
		txPool:          txPool,
		eventDispatcher: dispatcher,
	}
	m.generateBlock()
	go m.blockUpdater()
	return m
}

// blockUpdater is the goroutine for keep update mining block
func (m *MiningPool) blockUpdater() {
	for {
		select {
		case <-recommitTicker.C:
			m.generateBlock()

		case <-m.chain.BlockWaiter(m.chain.BestBlockHeight() + 1):
			// make a new commitMap, so that the expired map will be deleted(garbage-collected)
			m.commitMap = make(map[types.BlockCommitment]([]*types.Tx))
			m.generateBlock()

		case submitMsg := <-m.submitCh:
			err := m.submitWork(submitMsg.blockHeader)
			if err == nil {
				// make a new commitMap, so that the expired map will be deleted(garbage-collected)
				m.commitMap = make(map[types.BlockCommitment]([]*types.Tx))
				m.generateBlock()
			}
			submitMsg.reply <- err
		}
	}
}

// generateBlock generates a block template to mine
func (m *MiningPool) generateBlock() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	block, err := mining.NewBlockTemplate(m.chain, m.txPool, m.accountManager)
	if err != nil {
		log.Errorf("miningpool: failed on create NewBlockTemplate: %v", err)
		return
	}

	// block will not be nil here
	m.block = block
	m.commitMap[block.BlockCommitment] = block.Transactions
}

// GetWork will return a block header for p2p mining
func (m *MiningPool) GetWork() (*types.BlockHeader, error) {
	if m.block != nil {
		m.mutex.RLock()
		defer m.mutex.RUnlock()

		m.block.BlockHeader.Timestamp = uint64(time.Now().Unix())
		bh := m.block.BlockHeader
		return &bh, nil
	}
	return nil, errors.New("no block is ready for mining")
}

// SubmitWork will try to submit the result to the blockchain
func (m *MiningPool) SubmitWork(bh *types.BlockHeader) error {
	reply := make(chan error, 1)
	m.submitCh <- &submitBlockMsg{blockHeader: bh, reply: reply}
	err := <-reply
	if err != nil {
		log.WithFields(log.Fields{"err": err, "height": bh.Height}).Warning("submitWork failed")
	}
	return err
}

func (m *MiningPool) submitWork(bh *types.BlockHeader) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.block == nil || bh.PreviousBlockHash != m.block.PreviousBlockHash {
		return errors.New("pending mining block has been changed")
	}

	txs, ok := m.commitMap[bh.BlockCommitment]
	if !ok {
		return errors.New("BlockCommitment not found in history")
	}

	m.block.Transactions = txs
	m.block.BlockCommitment = bh.BlockCommitment
	m.block.Nonce = bh.Nonce
	m.block.Timestamp = bh.Timestamp

	isOrphan, err := m.chain.ProcessBlock(m.block)
	if err != nil {
		return err
	}
	if isOrphan {
		return errors.New("submit result is orphan")
	}

	if err := m.eventDispatcher.Post(event.NewMinedBlockEvent{Block: m.block}); err != nil {
		return err
	}

	return nil
}
