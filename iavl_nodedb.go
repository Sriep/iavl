package iavl

import (
	"bytes"
	"container/list"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/syndtr/goleveldb/leveldb/util"

	cmn "github.com/tendermint/tmlibs/common"
	dbm "github.com/tendermint/tmlibs/db"
)

type nodeDB struct {
	mtx        sync.Mutex               // Read/write lock.
	cache      map[string]*list.Element // Node cache.
	cacheSize  int                      // Node cache size limit in elements.
	cacheQueue *list.List               // LRU queue of cache elements. Used for deletion.
	db         dbm.DB                   // Persistent node storage.
	batch      dbm.Batch                // Batched writing buffer.
}

func newNodeDB(cacheSize int, db dbm.DB) *nodeDB {
	ndb := &nodeDB{
		cache:      make(map[string]*list.Element),
		cacheSize:  cacheSize,
		cacheQueue: list.New(),
		db:         db,
		batch:      db.NewBatch(),
	}
	return ndb
}

// GetNode gets a node from cache or disk. If it is an inner node, it does not
// load its children.
func (ndb *nodeDB) GetNode(hash []byte) *IAVLNode {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	// Check the cache.
	if elem, ok := ndb.cache[string(hash)]; ok {
		// Already exists. Move to back of cacheQueue.
		ndb.cacheQueue.MoveToBack(elem)
		return elem.Value.(*IAVLNode)
	}

	// Doesn't exist, load.
	buf := ndb.db.Get(hash)
	if len(buf) == 0 {
		cmn.PanicSanity(cmn.Fmt("Value missing for key %X", hash))
	}

	node, err := MakeIAVLNode(buf)
	if err != nil {
		cmn.PanicCrisis(cmn.Fmt("Error reading IAVLNode. bytes: %X, error: %v", buf, err))
	}

	node.hash = hash
	node.persisted = true
	ndb.cacheNode(node)

	return node
}

func (ndb *nodeDB) SaveNode(node *IAVLNode) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if node.hash == nil {
		cmn.PanicSanity("Expected to find node.hash, but none found.")
	}
	if node.persisted {
		cmn.PanicSanity("Shouldn't be calling save on an already persisted node.")
	}

	// Save node bytes to db
	buf := new(bytes.Buffer)
	if _, err := node.writeBytes(buf); err != nil {
		cmn.PanicCrisis(err)
	}
	ndb.batch.Set(node.hash, buf.Bytes())
	node.persisted = true
	ndb.cacheNode(node)

	// TODO: What do we do if this node's hash was previously orphaned?
	// Would have to be same key/val/version.
}

// NOTE: clears leftNode/rigthNode recursively
// NOTE: sets hashes recursively
func (ndb *nodeDB) SaveBranch(node *IAVLNode, version uint64) {
	if node.hash == nil {
		node.hash, _ = node.hashWithCount()
	}
	if node.persisted {
		return
	}

	// save children
	if node.leftNode != nil {
		ndb.SaveBranch(node.leftNode, version)
		node.leftNode = nil
	}
	if node.rightNode != nil {
		ndb.SaveBranch(node.rightNode, version)
		node.rightNode = nil
	}

	// save node
	node.version = version
	ndb.SaveNode(node)
}

var (
	// orphans/<version>/<hash>
	orphansPrefix    = "orphans/"
	orphansPrefixFmt = "orphans/%d/"
	orphansKeyFmt    = "orphans/%d/%x"
)

// Saves orphaned nodes to disk under a special prefix.
func (ndb *nodeDB) SaveOrphans(orphans []*IAVLNode) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	for _, node := range orphans {
		if !node.persisted {
			continue
		}
		if len(node.hash) == 0 {
			cmn.PanicSanity("Hash should not be empty")
		}
		key := fmt.Sprintf(orphansKeyFmt, node.version, node.hash)
		ndb.batch.Set([]byte(key), node.hash)
	}
}

func (ndb *nodeDB) DeleteOrphans(version uint64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.traverseOrphansVersion(version, func(key, value []byte) {
		ndb.batch.Delete(value)
		ndb.uncacheNode(value)
	})
}

func (ndb *nodeDB) traverseOrphans(fn func(k, v []byte)) {
	ndb.traverse(func(key, value []byte) {
		if strings.HasPrefix(string(key), orphansPrefix) {
			fn(key, value)
		}
	})
}

func (ndb *nodeDB) traverseOrphansVersion(version uint64, fn func(k, v []byte)) {
	prefix := fmt.Sprintf(orphansPrefixFmt, version)

	if ldb, ok := ndb.db.(*dbm.GoLevelDB); ok {
		// TODO: Test this code path.
		it := ldb.DB().NewIterator(util.BytesPrefix([]byte(prefix)), nil)
		for it.Next() {
			fn(it.Key(), it.Value())
		}
		it.Release()
		if err := it.Error(); err != nil {
			cmn.PanicSanity(err.Error())
		}
	} else {
		ndb.traverse(func(key, value []byte) {
			if strings.HasPrefix(string(key), prefix) {
				fn(key, value)
			}
		})
	}
}

func (ndb *nodeDB) uncacheNode(hash []byte) {
	if elem, ok := ndb.cache[string(hash)]; ok {
		ndb.cacheQueue.Remove(elem)
		delete(ndb.cache, string(hash))
	}
}

// Add a node to the cache and pop the least recently used node if we've
// reached the cache size limit.
func (ndb *nodeDB) cacheNode(node *IAVLNode) {
	elem := ndb.cacheQueue.PushBack(node)
	ndb.cache[string(node.hash)] = elem

	if ndb.cacheQueue.Len() > ndb.cacheSize {
		oldest := ndb.cacheQueue.Front()
		hash := ndb.cacheQueue.Remove(oldest).(*IAVLNode).hash
		delete(ndb.cache, string(hash))
	}
}

// Write to disk. Orphans are deleted here.
func (ndb *nodeDB) Commit() {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.batch.Write()
	ndb.batch = ndb.db.NewBatch()
}

///////////////////////////////////////////////////////////////////////////////

func (ndb *nodeDB) keys() []string {
	keys := []string{}

	ndb.traverse(func(key, value []byte) {
		keys = append(keys, string(key))
	})
	return keys
}

func (ndb *nodeDB) leafNodes() []*IAVLNode {
	leaves := []*IAVLNode{}

	ndb.traverseNodes(func(hash []byte, node *IAVLNode) {
		if node.isLeaf() {
			leaves = append(leaves, node)
		}
	})
	return leaves
}

func (ndb *nodeDB) orphans() [][]byte {
	orphans := [][]byte{}

	ndb.traverseOrphans(func(k, v []byte) {
		orphans = append(orphans, v)
	})
	return orphans
}

func (ndb *nodeDB) size() int {
	it := ndb.db.Iterator()
	size := 0

	for it.Next() {
		size++
	}
	return size
}

func (ndb *nodeDB) traverse(fn func(key, value []byte)) {
	it := ndb.db.Iterator()

	for it.Next() {
		fn(it.Key(), it.Value())
	}
}

func (ndb *nodeDB) traverseNodes(fn func(hash []byte, node *IAVLNode)) {
	nodes := []*IAVLNode{}

	ndb.traverse(func(key, value []byte) {
		if strings.HasPrefix(string(key), "orphans/") {
			return
		}
		node, err := MakeIAVLNode(value)
		if err != nil {
			cmn.PanicSanity("Couldn't decode node from database")
		}
		node.hash = key
		nodes = append(nodes, node)
	})

	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].hash, nodes[j].hash) < 0
	})

	for _, n := range nodes {
		fn(n.hash, n)
	}
}

func (ndb *nodeDB) String() string {
	var str string
	index := 0

	ndb.traverseOrphans(func(key, value []byte) {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
	})
	str += "\n"

	ndb.traverseNodes(func(hash []byte, node *IAVLNode) {
		if len(hash) == 0 {
			str += fmt.Sprintf("%d: <nil>\n", index)
		} else if node == nil {
			str += fmt.Sprintf("%d: %40x: <nil>\n", index, hash)
		} else if node.value == nil && node.height > 0 {
			str += fmt.Sprintf("%d: %40x: %s   %-16s h=%d version=%d\n", index, hash, node.key, "", node.height, node.version)
		} else {
			str += fmt.Sprintf("%d: %40x: %s = %-16s h=%d version=%d\n", index, hash, node.key, node.value, node.height, node.version)
		}
		index++
	})
	return "-" + "\n" + str + "-"
}
