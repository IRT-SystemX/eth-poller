package ingest

import (
	"context"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"log"
	"math/big"
	"reflect"
	"strconv"
	"sync"
	"time"
)

var (
	retry   = time.Duration(5)
	zero    = big.NewInt(0)
	one     = big.NewInt(1)
	ten     = big.NewInt(10)
	hundred = big.NewInt(100)
)

type rpcBlockHash struct {
	Hash common.Hash `json:"hash"`
}

type Engine struct {
	url            string
	client         *ethclient.Client
	rawClient      *rpc.Client
	start          *big.Int
	end            *big.Int
	syncMode       string
	syncThreadPool int
	syncThreadSize int
	synced         int64
	mux            sync.Mutex
	status         map[string]interface{}
	queue          chan BlockEvent
	connector      Connector
	processor      Processor
	fork           *ForkWatcher
}

func NewEngine(web3Socket string, syncMode string, syncThreadPool int, syncThreadSize int, maxForkSize int) *Engine {
	engine := &Engine{url: web3Socket,
		start:          big.NewInt(0),
		end:            big.NewInt(-1),
		syncMode:       syncMode,
		syncThreadPool: syncThreadPool,
		syncThreadSize: syncThreadSize,
		queue:          make(chan BlockEvent),
		status: map[string]interface{}{
			"connected": false,
			"sync":      "0%%",
			"current":   zero,
		},
	}
	engine.fork = NewForkWatcher(engine, maxForkSize)
	return engine
}

func (engine *Engine) Client() *ethclient.Client {
	return engine.client
}

func (engine *Engine) Status() map[string]interface{} {
	return engine.status
}

func (engine *Engine) Latest() (*big.Int, error) {
	header, err := engine.client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return nil, err
	} else {
		return header.Number, nil
	}
}

func (engine *Engine) SetStart(val string, plusOne bool) {
	if plusOne {
		start, _ := new(big.Int).SetString(val, 10)
		engine.start = new(big.Int).Add(start, one)
	} else {
		engine.start, _ = new(big.Int).SetString(val, 10)
	}
}

func (engine *Engine) SetEnd(val string) {
	engine.end, _ = new(big.Int).SetString(val, 10)
}

func (engine *Engine) SetConnector(connector Connector) {
	engine.connector = connector
}

func (engine *Engine) SetProcessor(processor Processor) {
	engine.processor = processor
}

func (engine *Engine) Connect() *ethclient.Client {
	for {
		rawClient, err := rpc.DialContext(context.Background(), engine.url)
		if err != nil {
			time.Sleep(retry * time.Second)
		} else {
			engine.client = ethclient.NewClient(rawClient)
			engine.rawClient = rawClient
			break
		}
	}
	engine.initialize()
	return engine.client
}

func (engine *Engine) initialize() {
	if engine.status["connected"] == false {
		engine.status["connected"] = true
		go func() {
			for {
				select {
				case blockEvent := <-engine.queue:
					if engine.connector != nil && !reflect.ValueOf(engine.connector).IsNil() {
						engine.connector.Apply(blockEvent)
					}
					engine.mux.Lock()
					engine.status["current"] = blockEvent.Number().String()
					engine.mux.Unlock()
				}
			}
		}()
	}
}

func (engine *Engine) process(number *big.Int) BlockEvent {
	block, err := engine.client.BlockByNumber(context.Background(), number)
	if err != nil {
		log.Println("Error block: ", err)
		return nil
	}
	var head rpcBlockHash
	err = engine.rawClient.CallContext(context.Background(), &head, "eth_getBlockByNumber", hexutil.EncodeBig(number), false)
	if err != nil {
		log.Println("Error block hash: ", err)
	}
	log.Printf("Process block #%s (%s) %s", block.Number().String(), time.Unix(int64(block.Time()), 0).Format("2006.01.02 15:04:05"), head.Hash.Hex())
	blockEvent := engine.processor.NewBlockEvent(block.Number(), block.ParentHash().Hex(), head.Hash.Hex())
	blockEvent.SetFork(false)
	if engine.processor != nil && !reflect.ValueOf(engine.processor).IsNil() {
		engine.processor.Process(block, blockEvent)
	}
	return blockEvent
}

func (engine *Engine) sync() {
	log.Printf("Syncing to block #%s", engine.end.String())
	if engine.end.Cmp(zero) == 0 {
		engine.synced = 100
		engine.status["sync"] = strconv.FormatInt(engine.synced, 10) + "%%"
		log.Printf("Synced %d", engine.synced)
	}
	if engine.syncMode == "normal" {
		engine.normalSync()
	} else if engine.syncMode == "fast" {
		engine.fastSync()
	} else {
		log.Fatal("Unknown sync mode %s", engine.syncMode)
	}
}

func (engine *Engine) normalSync() {
	for i := new(big.Int).Set(engine.start); i.Cmp(engine.end) < 0 || i.Cmp(engine.end) == 0; i.Add(i, one) {
		blockEvent := engine.process(i)
		if blockEvent != nil {
			engine.queue <- blockEvent
		}
		current := new(big.Int).Add(engine.start, i)
		if new(big.Int).Mod(current, ten).Cmp(zero) == 0 && current.Cmp(engine.end) != 0 {
			engine.printSync(current)
		}
	}
	engine.printSync(engine.end)
}

func (engine *Engine) fastSync() {
	size := new(big.Int).Sub(engine.end, engine.start)
	if size.Cmp(zero) > 0 {
		blockRange := big.NewInt(int64(engine.syncThreadPool * engine.syncThreadSize))
		iterMax := new(big.Int).Div(size, blockRange)
		for iter := big.NewInt(0); iter.Cmp(iterMax) < 0 || iter.Cmp(iterMax) == 0; iter.Add(iter, one) {
			begin := new(big.Int).Add(engine.start, new(big.Int).Mul(iter, blockRange))
			var wg sync.WaitGroup
			for k := 0; k < engine.syncThreadPool; k++ {
				threadBegin := new(big.Int).Add(begin, big.NewInt(int64(k*engine.syncThreadSize)))
				if threadBegin.Cmp(engine.end) <= 0 {
					wg.Add(1)
					go func(threadBegin *big.Int) {
						defer wg.Done()
						for j := 0; j < engine.syncThreadSize; j++ {
							i := new(big.Int).Add(threadBegin, big.NewInt(int64(j)))
							if i.Cmp(engine.end) > 0 {
								break
							}
							blockEvent := engine.process(i)
							if blockEvent != nil {
								engine.queue <- blockEvent
							}
						}
					}(threadBegin)
				}
			}
			wg.Wait()
			engine.printSync(new(big.Int).Add(begin, blockRange))
		}
	}
}

func (engine *Engine) printSync(current *big.Int) {
	if engine.end.Cmp(zero) > 0 {
		engine.synced = new(big.Int).Div(new(big.Int).Mul(current, hundred), engine.end).Int64()
	} else {
		engine.synced = 100
	}
	if engine.synced > 100 {
		engine.synced = 100
	}
	engine.mux.Lock()
	engine.status["sync"] = strconv.FormatInt(engine.synced, 10) + "%%"
	engine.mux.Unlock()
	log.Printf("Synced %d%%", engine.synced)
}

func (engine *Engine) Init() {
	last, err := engine.Latest()
	if err != nil {
		log.Fatal(err)
	}
	if engine.end.Cmp(zero) <= 0 {
		engine.end = last
	}
	engine.sync()
	engine.end = new(big.Int).Add(last, one)
}

func (engine *Engine) Listen() {
	headers := make(chan *types.Header)
	sub, err := engine.client.SubscribeNewHead(context.Background(), headers)
	if err != nil {
		log.Fatal(err)
	}
	for {
		select {
		case err := <-sub.Err():
			log.Println("Error: ", err)
		case header := <-headers:
			//log.Printf("New block #%s", header.Number.String())
			if header != nil {
				for i := new(big.Int).Set(engine.end); i.Cmp(header.Number) < 0 || i.Cmp(header.Number) == 0; i.Add(i, one) {
					blockEvent := engine.process(i)
					if blockEvent != nil {
						engine.fork.checkFork(blockEvent)
						engine.queue <- blockEvent
						engine.fork.apply(blockEvent)
					}
				}
				engine.end = new(big.Int).Add(header.Number, one)
			}
		}
	}
}
