package models

import (
	"fmt"

	"sync"

	"time"

	"math"

	"encoding/gob"

	"os"

	"github.com/SmartMeshFoundation/raiden-network/channel"
	"github.com/SmartMeshFoundation/raiden-network/transfer"
	"github.com/asdine/storm"
	gobcodec "github.com/asdine/storm/codec/gob"
	bolt "github.com/coreos/bbolt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

//export thread safe model
type ModelDB struct {
	db                      *storm.DB
	lock                    sync.Mutex
	NewTokenCallbacks       map[*NewTokenCb]bool
	NewChannelCallbacks     map[*ChannelCb]bool
	ChannelDepositCallbacks map[*ChannelCb]bool
	ChannelStateCallbacks   map[*ChannelCb]bool
	mlock                   sync.Mutex
}

type InternalEvent struct {
	ID            int `storm:"id,increment"`
	StateChangeId int
	BlockNumber   int64 `storm:"index"`
	EventObject   transfer.Event
}

type StateChange struct {
	ID          int `storm:"id,increment"`
	StateChange transfer.StateChange
}

type snapshotToWrite struct {
	ID            int
	StateChangeId int
	State         interface{}
}

var bucketEvents = []byte("events")
var bucketEventsBlock = []byte("eventsBlock")
var bucketStateChange = []byte("statechange")
var bucketSnapshot = "snapshot"
var bucketMeta = "meta"

const dbVersion = 1

func newModelDB() (db *ModelDB) {
	return &ModelDB{
		NewTokenCallbacks:       make(map[*NewTokenCb]bool),
		NewChannelCallbacks:     make(map[*ChannelCb]bool),
		ChannelDepositCallbacks: make(map[*ChannelCb]bool),
		ChannelStateCallbacks:   make(map[*ChannelCb]bool),
	}

}

func OpenDb(dbPath string) (model *ModelDB, err error) {
	model = newModelDB()
	needCreateDb := !common.FileExist(dbPath)
	var ver int
	model.db, err = storm.Open(dbPath, storm.BoltOptions(os.ModePerm, &bolt.Options{Timeout: 1 * time.Second}), storm.Codec(gobcodec.Codec))
	if err != nil {
		err = fmt.Errorf("cannot create or open db:%s,makesure you have write permission err:%v", dbPath, err)
		log.Crit(err.Error())
		return
	}
	if needCreateDb {
		err = model.db.Set(bucketMeta, "version", dbVersion)
		if err != nil {
			log.Crit(fmt.Sprintf("unable to create db "))
			return
		}
		//write a empty snapshot,
		model.db.Save(&snapshotToWrite{ID: 1})
		err = model.db.Set(bucketToken, keyToken, make(AddressMap))
		if err != nil {
			log.Crit(fmt.Sprintf("unable to create db "))
			return
		}
		model.initDb()
		model.MarkDbOpenedStatus()
	} else {
		err = model.db.Get(bucketMeta, "version", &ver)
		if err != nil {
			log.Crit(fmt.Sprintf("wrong db file format "))
			return
		}
		if ver != dbVersion {
			log.Crit("db version not match")
		}
		var closeFlag bool
		err = model.db.Get(bucketMeta, "close", &closeFlag)
		if err != nil {
			log.Crit(fmt.Sprintf("db meta data error"))
		}
		if closeFlag != true {
			log.Error("database not closed  last..., try to restore?")
		}
	}

	return
}

/*
第一步打开数据库
第二步检测是否正常关闭 IsDbCrashedLastTime
第三步 根据第二步的情况恢复数据
第四步 标记数据库可以正常处理数据了. MarkDbOpenedStatus
*/
func (model *ModelDB) MarkDbOpenedStatus() {
	model.db.Set(bucketMeta, "close", false)
}
func (model *ModelDB) IsDbCrashedLastTime() bool {
	var closeFlag bool
	err := model.db.Get(bucketMeta, "close", &closeFlag)
	if err != nil {
		log.Crit(fmt.Sprintf("db meta data error"))
	}
	return closeFlag != true
}
func (model *ModelDB) CloseDB() {
	model.lock.Lock()
	model.db.Set(bucketMeta, "close", true)
	model.db.Close()
	model.lock.Unlock()
}

//Log a state change and return its identifier
func (model *ModelDB) LogStateChange(stateChange transfer.StateChange) (id int, err error) {
	sc := &StateChange{
		StateChange: stateChange,
	}
	err = model.db.Save(sc)
	id = sc.ID
	return
}

// Log the events that were generated by `state_change_id` into the write ahead Log
func (model *ModelDB) LogEvents(stateChangeId int, events []transfer.Event, currentBlockNumber int64) error {
	for _, e := range events {
		err := model.db.Save(&InternalEvent{
			StateChangeId: stateChangeId,
			BlockNumber:   currentBlockNumber,
			EventObject:   e,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

/*
Get the raiden events in the period (inclusive) ranging from
        `from_block` to `to_block`.
*/
func (model *ModelDB) GetEventsInBlockRange(fromBlock, toBlock int64) (events []*InternalEvent, err error) {
	if fromBlock < 0 {
		fromBlock = 0
	}
	if toBlock < 0 {
		toBlock = math.MaxInt64
	}
	err = model.db.Range("BlockNumber", fromBlock, toBlock, &events)
	if err == storm.ErrNotFound { //ingore not found error
		err = nil
	}
	return
}

func (model *ModelDB) GetStateChangeById(id int) (st transfer.StateChange, err error) {
	var sc StateChange
	err = model.db.One("ID", id, &sc)
	if err != nil {
		return
	}
	st = sc.StateChange
	return
}

func (model *ModelDB) Snapshot(stateChangeId int, state interface{}) (id int, err error) {
	s := &snapshotToWrite{
		ID:            1,
		StateChangeId: stateChangeId,
		State:         state,
	}
	err = model.db.Update(s)
	return 1, err
}

func (model *ModelDB) LoadSnapshot() (state interface{}, err error) {
	var sw snapshotToWrite
	err = model.db.One("ID", 1, &sw)
	if err == nil {
		state = sw.State
	}
	if state == nil {
		err = storm.ErrNotFound
	}
	return
}
func init() {
	gob.Register(&InternalEvent{})
	gob.Register(&snapshotToWrite{})
	gob.Register(common.Address{})
}

func (model *ModelDB) initDb() {
	model.db.Init(&InternalEvent{})
	model.db.Init(&snapshotToWrite{})
	model.db.Init(&StateChange{})
	model.db.Init(&channel.ChannelSerialization{})
}
