package exchange

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/filecoin-project/go-hamt-ipld/v3"
	"github.com/filecoin-project/go-multistore"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	cbor "github.com/ipfs/go-ipld-cbor"
	cbg "github.com/whyrusleeping/cbor-gen"
)

//go:generate cbor-gen-for --map-encoding DataRef

// ErrRefNotFound is returned when a given ref is not in the store
var ErrRefNotFound = errors.New("ref not found")

// KIndex is the datastore key for persisting the index of a workdag
const KIndex = "idx"

// use 256 hash to prevent collision attacks
var hashOption = hamt.UseHashFunction(func(input []byte) []byte {
	res := sha256.Sum256(input)
	return res[:]
})

// Index contains the information about which objects are currently stored
// the key is a CID.String().
// It also implements a Least Frequently Used cache eviction mechanism to maintain storage withing given
// bounds inspired by https://github.com/dgrijalva/lfu-go.
// Content is garbage collected during eviction.
type Index struct {
	ms     *multistore.MultiStore
	ds     datastore.Batching
	root   *hamt.Node
	bstore blockstore.Blockstore
	store  cbor.IpldStore
	// Upper bound is the store usage amount afyer which we start evicting refs from the store
	ub uint64
	// Lower bound is the size we target when evicting to make room for new content
	// the interval between ub and lb is to try not evicting after every write once we reach ub
	lb uint64
	// updateFunc, if not nil, is called after every read transactions. The hook can be used
	// to trigger request for new content and refreshing the index with new popular content
	updateFunc func()

	mu sync.Mutex
	// current size of content committed to the store
	size uint64
	// linked list keeps track of all refs in least to most popular order to access as fast as possible
	blist *list.List
	// We still need to keep a map in memory
	Refs    map[string]*DataRef
	rootCID cid.Cid

	imu sync.Mutex
	// interest frequencies track the most popular content we don't have
	freqs *list.List
	// Interest is a map of interest ref pointers
	interest map[string]*DataRef
}

// DataRef encapsulates information about a content committed for storage
type DataRef struct {
	PayloadCID  cid.Cid
	PayloadSize int64
	StoreID     multistore.StoreID
	Freq        int64
	BucketID    int64
	// do not serialize
	bucketNode *list.Element
}

// IndexOption customizes the behavior of the index
type IndexOption func(*Index)

// WithBounds sets the upper and lower bounds of the LFU store
func WithBounds(up, lo uint64) IndexOption {
	return func(idx *Index) {
		// Should crash execution rather than running the index with sneaky bugs
		if up < lo {
			panic("upper bound cannot be lower than lower bound")
		}
		idx.ub = up
		idx.lb = lo
	}
}

// WithUpdateFunc sets an UpdateFunc callback and a read interval after which to call it
func WithUpdateFunc(fn func()) IndexOption {
	return func(idx *Index) {
		idx.updateFunc = fn
	}
}

// NewIndex creates a new Index instance, loading entries into a doubly linked list for faster read and writes
func NewIndex(ds datastore.Batching, ms *multistore.MultiStore, opts ...IndexOption) (*Index, error) {
	idx := &Index{
		blist:    list.New(),
		freqs:    list.New(),
		ds:       namespace.Wrap(ds, datastore.NewKey("/index")),
		ms:       ms,
		Refs:     make(map[string]*DataRef),
		interest: make(map[string]*DataRef),
		rootCID:  cid.Undef,
	}
	for _, o := range opts {
		o(idx)
	}
	// keep a reference of the blockstore for loading in graphsync
	idx.bstore = blockstore.NewBlockstore(idx.ds)
	idx.store = cbor.NewCborStore(idx.bstore)
	if err := idx.loadFromStore(); err != nil {
		return nil, err
	}

	// // Loads the ref frequencies in a doubly linked list for faster access
	err := idx.root.ForEach(context.TODO(), func(k string, val *cbg.Deferred) error {
		v := new(DataRef)
		if err := v.UnmarshalCBOR(bytes.NewReader(val.Raw)); err != nil {
			return err
		}
		idx.Refs[v.PayloadCID.String()] = v
		idx.size += uint64(v.PayloadSize)
		if e := idx.blist.Front(); e == nil {
			// insert the first element in the list
			li := newBucket(v.BucketID)
			li.entries[v] = 1
			v.bucketNode = idx.blist.PushFront(li)
			return nil
		}
		for e := idx.blist.Front(); e != nil; e = e.Next() {
			b := e.Value.(*bucket)
			if b.id == v.BucketID {
				b.entries[v] = 1
				v.bucketNode = e
				return nil
			}
			if b.id > v.BucketID {
				li := newBucket(v.BucketID)
				li.entries[v] = 1
				v.bucketNode = idx.blist.InsertBefore(li, e)
				return nil
			}
		}
		// if we're still here it means we're the highest ID in the list so we
		// insert it at the back
		li := newBucket(v.BucketID)
		li.entries[v] = 1
		v.bucketNode = idx.blist.PushBack(li)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return idx, nil
}

func (idx *Index) loadFromStore() error {
	// var err error
	enc, err := idx.ds.Get(datastore.NewKey(KIndex))
	if err != nil && errors.Is(err, datastore.ErrNotFound) {
		nd, err := hamt.NewNode(idx.store, hamt.UseTreeBitWidth(5), hashOption)
		if err != nil {
			return err
		}
		idx.root = nd
	} else if err != nil {
		return err
	}
	if err == nil {
		r, err := cid.Cast(enc)
		if err != nil {
			return err
		}
		idx.root, err = idx.LoadRoot(r, idx.store)
		if err != nil {
			return err
		}
		idx.rootCID = r
	}
	return nil
}

// LoadRoot loads a new HAMT root not from a given CID, it can be used to load a node
// from a different root than the current one for example
func (idx *Index) LoadRoot(r cid.Cid, store cbor.IpldStore) (*hamt.Node, error) {
	return hamt.LoadNode(context.TODO(), store, r, hamt.UseTreeBitWidth(5), hashOption)
}

// GetStoreID returns the StoreID of the store which has the given content
func (idx *Index) GetStoreID(id cid.Cid) (multistore.StoreID, error) {
	ref, err := idx.GetRef(id)
	if err != nil {
		return 0, err
	}
	return ref.StoreID, nil
}

// GetStore returns the store associated with a data CID
func (idx *Index) GetStore(id cid.Cid) (*multistore.Store, error) {
	storeID, err := idx.GetStoreID(id)
	if err != nil {
		return nil, err
	}
	return idx.ms.Get(storeID)
}

// Root returns the HAMT root CID
func (idx *Index) Root() cid.Cid {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.rootCID
}

// Available returns the storage capacity still available or 0 if full
// a margin set by lower bound (lb) provides leeway for the eviction algorithm
func (idx *Index) Available() uint64 {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	margin := idx.ub - idx.lb
	if idx.ub-idx.size < margin {
		return 0
	}
	return idx.ub - idx.size
}

// Flush persists the Refs to the store, callers must take care of the mutex
// context is not actually used downstream so we use a TODO()
func (idx *Index) Flush() error {
	if err := idx.root.Flush(context.TODO()); err != nil {
		return err
	}
	r, err := idx.store.Put(context.TODO(), idx.root)
	if err != nil {
		return err
	}
	idx.rootCID = r
	return idx.ds.Put(datastore.NewKey(KIndex), r.Bytes())
}

// DropRef removes all content linked to a root CID and associated Refs
func (idx *Index) DropRef(k cid.Cid) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if found, err := idx.root.Delete(context.TODO(), k.String()); err != nil {
		return err
	} else if !found {
		return ErrRefNotFound
	}
	ref := idx.Refs[k.String()]
	idx.remBlistEntry(ref.bucketNode, ref)

	err := idx.ms.Delete(ref.StoreID)
	if err != nil {
		return err
	}

	delete(idx.Refs, k.String())
	return idx.Flush()
}

// SetRef adds a ref in the index and increments the LFU queue
func (idx *Index) SetRef(ref *DataRef) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	k := ref.PayloadCID.String()
	idx.Refs[k] = ref
	idx.size += uint64(ref.PayloadSize)
	if idx.ub > 0 && idx.lb > 0 {
		if idx.size > idx.ub {
			idx.evict(idx.size - idx.lb)
		}
	}
	// We evict the item before adding the new one
	idx.increment(ref)
	if err := idx.root.Set(context.TODO(), k, ref); err != nil {
		return err
	}
	return idx.Flush()
}

// GetRef gets a ref in the index for a given root CID and increments the LFU list registering a Read
func (idx *Index) GetRef(k cid.Cid) (*DataRef, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	ref, ok := idx.Refs[k.String()]
	if !ok {
		return nil, ErrRefNotFound
	}
	idx.increment(ref)
	// Update the freq
	if err := idx.root.Set(context.TODO(), k.String(), ref); err != nil {
		return nil, err
	}
	return ref, idx.Flush()
}

// PeekRef returns a ref from the index without actually registering a read in the LFU
func (idx *Index) PeekRef(k cid.Cid) (*DataRef, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	ref := new(DataRef)
	ref, ok := idx.Refs[k.String()]
	if !ok {
		return nil, ErrRefNotFound
	}
	return ref, nil
}

// ListRefs returns all the content refs currently stored on this node as well as their read frequencies
func (idx *Index) ListRefs() ([]*DataRef, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	refs := make([]*DataRef, len(idx.Refs))
	i := 0
	for e := idx.blist.Front(); e != nil; e = e.Next() {
		for k := range e.Value.(*bucket).entries {
			refs[i] = k
			i++
		}
	}
	return refs, nil
}

// Len returns the number of roots this index is currently storing
func (idx *Index) Len() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return len(idx.Refs)
}

// Bstore returns the lower level blockstore storing the hamt
func (idx *Index) Bstore() blockstore.Blockstore {
	return idx.bstore
}

type bucket struct {
	id      int64
	entries map[*DataRef]byte
}

func newBucket(id int64) *bucket {
	return &bucket{
		id:      id,
		entries: make(map[*DataRef]byte),
	}
}

func (idx *Index) increment(ref *DataRef) {
	currentPlace := ref.bucketNode
	var nextID int64
	var nextPlace *list.Element
	if currentPlace == nil {
		// new entry
		nextID = 1
		nextPlace = idx.blist.Back()
		if nextPlace != nil {
			nextID = nextPlace.Value.(*bucket).id
		}
	} else {
		// move up
		nextID = currentPlace.Value.(*bucket).id + 1
		nextPlace = currentPlace.Next()
	}

	if nextPlace == nil || nextPlace.Value.(*bucket).id != nextID {
		// create a new list entry
		li := &bucket{
			id:      nextID,
			entries: make(map[*DataRef]byte),
		}
		if currentPlace != nil {
			nextPlace = idx.blist.InsertAfter(li, currentPlace)
		} else {
			nextPlace = idx.blist.PushFront(li)
		}
	}
	// frequency starts at 0 and only increments after it was placed in the list
	if currentPlace != nil {
		ref.Freq++
	}
	ref.BucketID = nextID
	ref.bucketNode = nextPlace
	nextPlace.Value.(*bucket).entries[ref] = 1
	if currentPlace != nil {
		// remove from current position
		idx.remBlistEntry(currentPlace, ref)
	}
}

func (idx *Index) remBlistEntry(place *list.Element, entry *DataRef) {
	b := place.Value.(*bucket)
	delete(b.entries, entry)
	if len(b.entries) == 0 {
		idx.blist.Remove(place)
	}
}

func (idx *Index) remFreqEntry(place *list.Element, entry *DataRef) {
	b := place.Value.(*listEntry)
	delete(b.entries, entry)
	if len(b.entries) == 0 {
		idx.freqs.Remove(place)
	}
}

func (idx *Index) evict(size uint64) uint64 {
	// No lock here so it can be called
	// from within the lock (during Set)
	var evicted uint64
	for place := idx.blist.Front(); place != nil; place = place.Next() {
		for entry := range place.Value.(*bucket).entries {
			delete(idx.Refs, entry.PayloadCID.String())

			err := idx.ms.Delete(entry.StoreID)
			if err != nil {
				continue
			}

			idx.remBlistEntry(place, entry)
			evicted += uint64(entry.PayloadSize)
			idx.size -= uint64(entry.PayloadSize)
			if evicted >= size {
				return evicted
			}
		}
	}
	return evicted
}

// ---------- Interest --------------

type listEntry struct {
	entries map[*DataRef]byte
	freq    int64
}

func newListEntry(freq int64) *listEntry {
	return &listEntry{
		entries: make(map[*DataRef]byte),
		freq:    freq,
	}
}

// LoadInterest loads potential new content in a different doubly linked list
// in this situation the most popular content is at the back of the list
func (idx *Index) LoadInterest(r cid.Cid, store cbor.IpldStore) error {
	root, err := idx.LoadRoot(r, store)
	if err != nil {
		return err
	}

	idx.imu.Lock()
	defer idx.imu.Unlock()
	return root.ForEach(context.TODO(), func(k string, val *cbg.Deferred) error {
		idx.mu.Lock()
		if _, ok := idx.Refs[k]; ok {
			// If we already have it skip it
			return nil
		}
		idx.mu.Unlock()

		v := new(DataRef)
		if err := v.UnmarshalCBOR(bytes.NewReader(val.Raw)); err != nil {
			return err
		}

		// Check if this ref already is in the interest list
		if ref, ok := idx.interest[k]; ok {
			currentPlace := ref.bucketNode
			// If it is, add the freqs
			nextFreq := ref.Freq + v.Freq
			// sometimes a node may have content with 0 reads in their index
			if nextFreq == ref.Freq {
				// no need to do anything
				return nil
			}
			if nextFreq != ref.Freq {
				// After we're done moving things around we can remove the previous entry
				defer idx.remFreqEntry(currentPlace, ref)
			}
			ref.Freq = nextFreq
			// starting from the current position iterate until either reaching the right bucket
			// or a higher bucket
			for np := ref.bucketNode; np != nil; np = np.Next() {
				le := np.Value.(*listEntry)
				if le.freq == nextFreq {
					le.entries[ref] = 1
					ref.bucketNode = np
					return nil
				}
				// create a new bucket and insert it before the higher one
				if le.freq > nextFreq {
					e := newListEntry(nextFreq)
					e.entries[ref] = 1
					ref.bucketNode = idx.freqs.InsertBefore(e, np)
					return nil
				}
			}
			le := newListEntry(nextFreq)
			le.entries[ref] = 1
			ref.bucketNode = idx.freqs.PushBack(le)
			return nil
		}

		idx.interest[k] = v
		if e := idx.freqs.Front(); e == nil {
			// insert the first element in the list
			li := newListEntry(v.Freq)
			li.entries[v] = 1
			v.bucketNode = idx.freqs.PushFront(li)
			return nil
		}
		for e := idx.freqs.Front(); e != nil; e = e.Next() {
			le := e.Value.(*listEntry)
			if le.freq == v.Freq {
				le.entries[v] = 1
				v.bucketNode = e
				return nil
			}
			if le.freq > v.Freq {
				li := newListEntry(v.Freq)
				li.entries[v] = 1
				v.bucketNode = idx.freqs.InsertBefore(li, e)
				return nil
			}
		}
		// if we're still here it means we're the highest frequency in the list so we
		// insert it at the back
		li := newListEntry(v.Freq)
		li.entries[v] = 1
		v.bucketNode = idx.freqs.PushBack(li)
		return nil
	})
}

// Interesting returns a bucket of most interesting refs in the index that could be retrieved to improve
// the local index
func (idx *Index) Interesting() (map[*DataRef]byte, error) {
	idx.imu.Lock()
	defer idx.imu.Unlock()
	av := idx.Available()
	added := uint64(0)
	out := make(map[*DataRef]byte)
	// If we have space fill the tank
	if av > 0 {
		for e := idx.freqs.Back(); e != nil; e = e.Prev() {
			for k, v := range e.Value.(*listEntry).entries {
				out[k] = v
				added += uint64(k.PayloadSize)
				if added >= av {
					return out, nil
				}
			}
		}
		// might not have enough to fill all the space and that's fine
		return out, nil
	}
	// get the front bucket which is the least frequently accessed
	front := idx.blist.Front()
	// start from the back which is the most frequently used
	if e := idx.freqs.Back(); e != nil {
		entry := e.Value.(*listEntry)
		for ref := range front.Value.(*bucket).entries {
			if entry.freq > ref.Freq {
				// return the first entry for now
				for k, v := range entry.entries {
					out[k] = v
					return out, nil
				}
			}
		}
	}
	return nil, errors.New("nothing interesting")
}

// InterestLen returns the number of interesting refs in our index
func (idx *Index) InterestLen() int {
	idx.imu.Lock()
	defer idx.imu.Unlock()
	return len(idx.interest)
}

// DropInterest removes a ref from the interest list
func (idx *Index) DropInterest(k cid.Cid) error {
	idx.imu.Lock()
	defer idx.imu.Unlock()
	ref, ok := idx.interest[k.String()]
	if !ok {
		return errors.New("ref not found")
	}
	delete(idx.interest, k.String())
	idx.remFreqEntry(ref.bucketNode, ref)
	return nil
}
