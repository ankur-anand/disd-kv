package kv

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/ankur-anand/dis-db/proto/v1/raftkv"
	badger "github.com/dgraph-io/badger/v3"
)

const (
	// Default BadgerDB discardRatio. It represents the discard ratio for the
	// BadgerDB GC.
	//
	// Ref: https://godoc.org/github.com/dgraph-io/badger#DB.RunValueLogGC
	badgerDiscardRatio = 0.5

	// Default BadgerDB GC interval
	badgerGCInterval = 10 * time.Minute
)

// Database is a wrapper around a BadgerDB backend database
type Database struct {
	db         *badger.DB // the underlying databse
	ctx        context.Context
	cancelFunc context.CancelFunc
}

// NewDatabase returns a new initialized BadgerDB database
// If the database cannot be initialized, an error will be returned.
func NewDatabase(ctx context.Context, dataDir string) (Database, error) {
	if err := os.MkdirAll(dataDir, 0774); err != nil {
		return Database{}, err
	}

	opts := badger.DefaultOptions(dataDir)
	opts.SyncWrites = true
	opts.Dir, opts.ValueDir = dataDir, dataDir

	badgerDB, err := badger.Open(opts)
	if err != nil {
		return Database{}, err
	}

	bdb := Database{
		db: badgerDB,
	}
	// context with cancel from parent
	bdb.ctx, bdb.cancelFunc = context.WithCancel(ctx)
	go bdb.runGC(bdb.ctx)
	return bdb, nil
}

// Shutdown tries to close if any running background goroutines.
func (d Database) Shutdown() {
	d.cancelFunc()
}

// GetB attempts to get a value for a given bytes key.
// If the key does not exist it returns a nil value.
func (d Database) GetB(key []byte) ([]byte, error) {

	// View is a closure.
	var value []byte
	err := d.db.View(func(txn *badger.Txn) error {

		item, err := txn.Get(key)

		if err != nil {
			return err //
		}

		// Copy the value as the value provided Badger is only valid while the
		// transaction is open.
		return item.Value(func(val []byte) error {
			value = make([]byte, len(val))
			copy(value, val)
			return nil
		})

	})

	if err == badger.ErrKeyNotFound {
		return nil, nil
	}

	return value, err
}

// SetB attempts to store a value for a given bytes key
func (d Database) SetB(key, val []byte) error {
	txn := d.db.NewTransaction(true)
	err := txn.Set([]byte(key), val)
	if err == badger.ErrTxnTooBig {
		_ = txn.Commit()
		txn = d.db.NewTransaction(true)
		err = txn.Set(key, val)
	}
	return txn.Commit()
}

// DelB deletes the given bytes key from the underlying database.
func (d Database) DelB(key []byte) error {
	txn := d.db.NewTransaction(true)
	defer func() {
		_ = txn.Commit()
	}()

	err := txn.Delete(key)
	if err != nil {
		return err
	}

	return txn.Commit()
}

// runGC triggers the garbage collection for the BadgerDB backend database. It
// should be run in a goroutine.
func (d Database) runGC(ctx context.Context) {
	ticker := time.NewTicker(badgerGCInterval)
	for {
		select {
		case <-ticker.C:
			err := d.db.RunValueLogGC(badgerDiscardRatio)
			if err != nil {
				// don't report error when GC didn't result in any cleanup
				if err == badger.ErrNoRewrite {
					log.Printf("no BadgerDB GC occurred: %v", err)
				} else {
					log.Printf("failed to GC BadgerDB: %v", err)
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

// SnapshotItems provides a snapshot isolation of a transaction
// from the underyling database
func (d Database) SnapshotItems() <-chan *raftkv.SnapshotItem {
	// create a new no blocking channel
	ch := make(chan *raftkv.SnapshotItem, 1024)
	// generate items from snapshot to channel
	go d.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10
		it := txn.NewIterator(opts)
		defer it.Close()

		keyCount := 0
		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			ssi := &raftkv.SnapshotItem{Key: k, Value: v}
			copy(ssi.Key, k)
			keyCount = keyCount + 1
			ch <- ssi
			if err != nil {
				return err
			}
		}

		// just use nil to mark the end
		ssi := &raftkv.SnapshotItem{
			Key:   nil,
			Value: nil,
		}
		ch <- ssi

		log.Printf("total number of keys in this snapshot = %d", keyCount)

		return nil
	})

	// return channel to persist
	return ch
}
