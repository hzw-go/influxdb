package tsm1

import (
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/tsdb"
	"github.com/influxdata/influxql"
	"go.uber.org/zap"
)

// ringShards specifies the number of partitions that the hash ring used to
// store the entry mappings contains. It must be a power of 2. From empirical
// testing, a value above the number of cores on the machine does not provide
// any additional benefit. For now we'll set it to the number of cores on the
// largest box we could imagine running influx.
const ringShards = 16

var (
	// ErrSnapshotInProgress is returned if a snapshot is attempted while one is already running.
	ErrSnapshotInProgress = fmt.Errorf("snapshot in progress")
)

// ErrCacheMemorySizeLimitExceeded returns an error indicating an operation
// could not be completed due to exceeding the cache-max-memory-size setting.
func ErrCacheMemorySizeLimitExceeded(n, limit uint64) error {
	return fmt.Errorf("cache-max-memory-size exceeded: (%d/%d)", n, limit)
}

// entry is a set of values and some metadata.
type entry struct {
	mu     sync.RWMutex
	values Values // All stored values.

	// The type of values stored. Read only so doesn't need to be protected by
	// mu.
	vtype byte
}

// newEntryValues returns a new instance of entry with the given values.  If the
// values are not valid, an error is returned.
func newEntryValues(values []Value) (*entry, error) {
	e := &entry{}
	e.values = make(Values, 0, len(values))
	e.values = append(e.values, values...)

	// No values, don't check types and ordering
	if len(values) == 0 {
		return e, nil
	}

	et := valueType(values[0])
	for _, v := range values {
		// Make sure all the values are the same type
		if et != valueType(v) {
			return nil, tsdb.ErrFieldTypeConflict
		}
	}

	// Set the type of values stored.
	e.vtype = et

	return e, nil
}

// add adds the given values to the entry.
// 追加写
// todo 是否需要保证顺序
func (e *entry) add(values []Value) error {
	if len(values) == 0 {
		return nil // Nothing to do.
	}

	// Are any of the new values the wrong type?
	if e.vtype != 0 {
		for _, v := range values {
			if e.vtype != valueType(v) {
				return tsdb.ErrFieldTypeConflict
			}
		}
	}

	// entry currently has no values, so add the new ones and we're done.
	e.mu.Lock()
	if len(e.values) == 0 {
		e.values = values
		e.vtype = valueType(values[0])
		e.mu.Unlock()
		return nil
	}

	// Append the new values to the existing ones...
	e.values = append(e.values, values...)
	e.mu.Unlock()
	return nil
}

// deduplicate sorts and orders the entry's values. If values are already deduped and sorted,
// the function does no work and simply returns.
func (e *entry) deduplicate() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(e.values) <= 1 {
		return
	}
	e.values = e.values.Deduplicate()
}

// count returns the number of values in this entry.
func (e *entry) count() int {
	e.mu.RLock()
	n := len(e.values)
	e.mu.RUnlock()
	return n
}

// filter removes all values with timestamps between min and max inclusive.
// 删除时间戳在[min, max]的points
func (e *entry) filter(min, max int64) {
	e.mu.Lock()
	if len(e.values) > 1 {
		e.values = e.values.Deduplicate()
	}
	e.values = e.values.Exclude(min, max)
	e.mu.Unlock()
}

// size returns the size of this entry in bytes.
func (e *entry) size() int {
	e.mu.RLock()
	sz := e.values.Size()
	e.mu.RUnlock()
	return sz
}

// InfluxQLType returns for the entry the data type of its values.
func (e *entry) InfluxQLType() (influxql.DataType, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.values.InfluxQLType()
}

// Statistics gathered by the Cache.
const (
	// levels - point in time measures

	statCacheMemoryBytes = "memBytes"      // level: Size of in-memory cache in bytes
	statCacheDiskBytes   = "diskBytes"     // level: Size of on-disk snapshots in bytes
	statSnapshots        = "snapshotCount" // level: Number of active snapshots.
	statCacheAgeMs       = "cacheAgeMs"    // level: Number of milliseconds since cache was last snapshoted at sample time

	// counters - accumulative measures

	statCachedBytes         = "cachedBytes"         // counter: Total number of bytes written into snapshots.
	statWALCompactionTimeMs = "WALCompactionTimeMs" // counter: Total number of milliseconds spent compacting snapshots

	statCacheWriteOK      = "writeOk"
	statCacheWriteErr     = "writeErr"
	statCacheWriteDropped = "writeDropped"
)

// storer is the interface that descibes a cache's store.
// where point cache is actually store
type storer interface {
	entry(key []byte) *entry                        // Get an entry by its key.
	write(key []byte, values Values) (bool, error)  // Write an entry to the store.
	add(key []byte, entry *entry)                   // Add a new entry to the store.
	remove(key []byte)                              // Remove an entry from the store.
	keys(sorted bool) [][]byte                      // Return an optionally sorted slice of entry keys.
	apply(f func([]byte, *entry) error) error       // Apply f to all entries in the store in parallel.
	applySerial(f func([]byte, *entry) error) error // Apply f to all entries in serial.
	reset()                                         // Reset the store to an initial unused state.
	split(n int) []storer                           // Split splits the store into n stores
	count() int                                     // Count returns the number of keys in the store
}

// Cache maintains an in-memory store of Values for a set of keys.
type Cache struct {
	// Due to a bug in atomic  size needs to be the first word in the struct, as
	// that's the only place where you're guaranteed to be 64-bit aligned on a
	// 32 bit system. See: https://golang.org/pkg/sync/atomic/#pkg-note-BUG
	size         uint64
	snapshotSize uint64

	mu      sync.RWMutex
	store   storer
	maxSize uint64

	// snapshots are the cache objects that are currently being written to tsm files
	// they're kept in memory while flushing so they can be queried along with the cache.
	// they are read only and should never be modified
	snapshot     *Cache
	snapshotting bool

	// This number is the number of pending or failed WriteSnaphot attempts since the last successful one.
	snapshotAttempts int

	stats         *CacheStatistics
	lastSnapshot  time.Time
	lastWriteTime time.Time

	// A one time synchronization used to initial the cache with a store.  Since the store can allocate a
	// a large amount memory across shards, we lazily create it.
	initialize       atomic.Value
	initializedCount uint32
}

// NewCache returns an instance of a cache which will use a maximum of maxSize bytes of memory.
// Only used for engine caches, never for snapshots.
func NewCache(maxSize uint64) *Cache {
	c := &Cache{
		maxSize:      maxSize,
		store:        emptyStore{},
		stats:        &CacheStatistics{},
		lastSnapshot: time.Now(),
	}
	c.initialize.Store(&sync.Once{})
	c.UpdateAge()
	c.UpdateCompactTime(0)
	c.updateCachedBytes(0)
	c.updateMemSize(0)
	c.updateSnapshots()
	return c
}

// CacheStatistics hold statistics related to the cache.
type CacheStatistics struct {
	MemSizeBytes        int64
	DiskSizeBytes       int64
	SnapshotCount       int64
	CacheAgeMs          int64
	CachedBytes         int64
	WALCompactionTimeMs int64
	WriteOK             int64
	WriteErr            int64
	WriteDropped        int64
}

// Statistics returns statistics for periodic monitoring.
func (c *Cache) Statistics(tags map[string]string) []models.Statistic {
	return []models.Statistic{{
		Name: "tsm1_cache",
		Tags: tags,
		Values: map[string]interface{}{
			statCacheMemoryBytes:    atomic.LoadInt64(&c.stats.MemSizeBytes),
			statCacheDiskBytes:      atomic.LoadInt64(&c.stats.DiskSizeBytes),
			statSnapshots:           atomic.LoadInt64(&c.stats.SnapshotCount),
			statCacheAgeMs:          atomic.LoadInt64(&c.stats.CacheAgeMs),
			statCachedBytes:         atomic.LoadInt64(&c.stats.CachedBytes),
			statWALCompactionTimeMs: atomic.LoadInt64(&c.stats.WALCompactionTimeMs),
			statCacheWriteOK:        atomic.LoadInt64(&c.stats.WriteOK),
			statCacheWriteErr:       atomic.LoadInt64(&c.stats.WriteErr),
			statCacheWriteDropped:   atomic.LoadInt64(&c.stats.WriteDropped),
		},
	}}
}

// init initializes the cache and allocates the underlying store.  Once initialized,
// the store re-used until Freed.
func (c *Cache) init() {
	if !atomic.CompareAndSwapUint32(&c.initializedCount, 0, 1) {
		return
	}

	c.mu.Lock()
	c.store, _ = newring(ringShards)
	c.mu.Unlock()
}

// Free releases the underlying store and memory held by the Cache.
func (c *Cache) Free() {
	if !atomic.CompareAndSwapUint32(&c.initializedCount, 1, 0) {
		return
	}

	c.mu.Lock()
	c.store = emptyStore{}
	c.mu.Unlock()
}

// Write writes the set of values for the key to the cache. This function is goroutine-safe.
// It returns an error if the cache will exceed its max size by adding the new values.
func (c *Cache) Write(key []byte, values []Value) error {
	c.init()
	addedSize := uint64(Values(values).Size())

	// Enough room in the cache?
	// 提前判断cache有足够空间
	limit := c.maxSize
	n := c.Size() + addedSize

	if limit > 0 && n > limit {
		atomic.AddInt64(&c.stats.WriteErr, 1)
		return ErrCacheMemorySizeLimitExceeded(n, limit)
	}

	newKey, err := c.store.write(key, values)
	if err != nil {
		atomic.AddInt64(&c.stats.WriteErr, 1)
		return err
	}

	if newKey {
		addedSize += uint64(len(key))
	}
	// Update the cache size and the memory size stat.
	c.increaseSize(addedSize)
	c.updateMemSize(int64(addedSize))
	atomic.AddInt64(&c.stats.WriteOK, 1)

	return nil
}

// WriteMulti writes the map of keys and associated values to the cache. This
// function is goroutine-safe. It returns an error if the cache will exceeded
// its max size by adding the new values.  The write attempts to write as many
// values as possible.  If one key fails, the others can still succeed and an
// error will be returned.
func (c *Cache) WriteMulti(values map[string][]Value) error {
	c.init()
	var addedSize uint64
	for _, v := range values {
		addedSize += uint64(Values(v).Size())
	}

	// Enough room in the cache?
	limit := c.maxSize // maxSize is safe for reading without a lock.
	n := c.Size() + addedSize
	if limit > 0 && n > limit {
		atomic.AddInt64(&c.stats.WriteErr, 1)
		return ErrCacheMemorySizeLimitExceeded(n, limit)
	}

	var werr error
	c.mu.RLock()
	store := c.store
	c.mu.RUnlock()

	// We'll optimistially set size here, and then decrement it for write errors.
	c.increaseSize(addedSize)
	for k, v := range values {
		newKey, err := store.write([]byte(k), v)
		if err != nil {
			// The write failed, hold onto the error and adjust the size delta.
			werr = err
			addedSize -= uint64(Values(v).Size())
			c.decreaseSize(uint64(Values(v).Size()))
		}
		if newKey {
			addedSize += uint64(len(k))
			c.increaseSize(uint64(len(k)))
		}
	}

	// Some points in the batch were dropped.  An error is returned so
	// error stat is incremented as well.
	if werr != nil {
		atomic.AddInt64(&c.stats.WriteDropped, 1)
		atomic.AddInt64(&c.stats.WriteErr, 1)
	}

	// Update the memory size stat
	c.updateMemSize(int64(addedSize))
	atomic.AddInt64(&c.stats.WriteOK, 1)

	// that's why batch writing is faster
	c.mu.Lock()
	c.lastWriteTime = time.Now()
	c.mu.Unlock()

	return werr
}

// Snapshot takes a snapshot of the current cache, adds it to the slice of caches that
// are being flushed, and resets the current cache with new values.
// cache无可用空间时，执行snapshot
func (c *Cache) Snapshot() (*Cache, error) {
	c.init()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.snapshotting {
		return nil, ErrSnapshotInProgress
	}

	c.snapshotting = true
	c.snapshotAttempts++ // increment the number of times we tried to do this

	// If no snapshot exists, create a new one, otherwise update the existing snapshot
	if c.snapshot == nil {
		store, err := newring(ringShards)
		if err != nil {
			return nil, err
		}

		c.snapshot = &Cache{
			store: store,
		}
	}

	// Did a prior snapshot exist that failed?  If so, return the existing
	// snapshot to retry.
	if c.snapshot.Size() > 0 {
		return c.snapshot, nil
	}

	// there are two store in cache: cache.store corresponding to the lsm's memory, snapshot.store corresponding to the lsm's immutable memory
	// how to snapshot the cache?
	// 1,move the cache.store to snapshot.store
	// 2,make a new store and set it to cache.store
	// 重新申请一块内存作为cache，原始的cache作为snapshot（lsm中的immutable memory），等待compaction
	c.snapshot.store, c.store = c.store, c.snapshot.store
	snapshotSize := c.Size()

	// Save the size of the snapshot on the snapshot cache
	atomic.StoreUint64(&c.snapshot.size, snapshotSize)
	// Save the size of the snapshot on the live cache
	atomic.StoreUint64(&c.snapshotSize, snapshotSize)

	// Reset the cache's store.
	c.store.reset()
	atomic.StoreUint64(&c.size, 0)
	c.lastSnapshot = time.Now()

	c.updateCachedBytes(snapshotSize) // increment the number of bytes added to the snapshot
	c.updateSnapshots()

	return c.snapshot, nil
}

// Deduplicate sorts the snapshot before returning it. The compactor and any queries
// coming in while it writes will need the values sorted.
func (c *Cache) Deduplicate() {
	c.mu.RLock()
	store := c.store
	c.mu.RUnlock()

	// Apply a function that simply calls deduplicate on each entry in the ring.
	// apply cannot return an error in this invocation.
	_ = store.apply(func(_ []byte, e *entry) error { e.deduplicate(); return nil })
}

// ClearSnapshot removes the snapshot cache from the list of flushing caches and
// adjusts the size.
func (c *Cache) ClearSnapshot(success bool) {
	c.init()

	c.mu.RLock()
	snapStore := c.snapshot.store
	c.mu.RUnlock()

	// reset the snapshot store outside of the write lock
	if success {
		snapStore.reset()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.snapshotting = false

	if success {
		c.snapshotAttempts = 0
		c.updateMemSize(-int64(atomic.LoadUint64(&c.snapshotSize))) // decrement the number of bytes in cache

		// Reset the snapshot to a fresh Cache.
		c.snapshot = &Cache{
			store: c.snapshot.store,
		}

		atomic.StoreUint64(&c.snapshotSize, 0)
		c.updateSnapshots()
	}
}

// Size returns the number of point-calcuated bytes the cache currently uses.
func (c *Cache) Size() uint64 {
	return atomic.LoadUint64(&c.size) + atomic.LoadUint64(&c.snapshotSize)
}

// increaseSize increases size by delta.
func (c *Cache) increaseSize(delta uint64) {
	atomic.AddUint64(&c.size, delta)
}

// decreaseSize decreases size by delta.
func (c *Cache) decreaseSize(delta uint64) {
	// Per sync/atomic docs, bit-flip delta minus one to perform subtraction within AddUint64.
	atomic.AddUint64(&c.size, ^(delta - 1))
}

// MaxSize returns the maximum number of bytes the cache may consume.
func (c *Cache) MaxSize() uint64 {
	return c.maxSize
}

func (c *Cache) Count() int {
	c.mu.RLock()
	n := c.store.count()
	c.mu.RUnlock()
	return n
}

// Keys returns a sorted slice of all keys under management by the cache.
func (c *Cache) Keys() [][]byte {
	c.mu.RLock()
	store := c.store
	c.mu.RUnlock()
	return store.keys(true)
}

func (c *Cache) Split(n int) []*Cache {
	if n == 1 {
		return []*Cache{c}
	}

	caches := make([]*Cache, n)
	storers := c.store.split(n)
	for i := 0; i < n; i++ {
		caches[i] = &Cache{
			store: storers[i],
		}
	}
	return caches
}

// Type returns the series type for a key.
func (c *Cache) Type(key []byte) (models.FieldType, error) {
	c.mu.RLock()
	e := c.store.entry(key)
	if e == nil && c.snapshot != nil {
		e = c.snapshot.store.entry(key)
	}
	c.mu.RUnlock()

	if e != nil {
		typ, err := e.InfluxQLType()
		if err != nil {
			return models.Empty, tsdb.ErrUnknownFieldType
		}

		switch typ {
		case influxql.Float:
			return models.Float, nil
		case influxql.Integer:
			return models.Integer, nil
		case influxql.Unsigned:
			return models.Unsigned, nil
		case influxql.Boolean:
			return models.Boolean, nil
		case influxql.String:
			return models.String, nil
		}
	}

	return models.Empty, tsdb.ErrUnknownFieldType
}

// Values returns a copy of all values, deduped and sorted, for the given key.
func (c *Cache) Values(key []byte) Values {
	var snapshotEntries *entry

	c.mu.RLock()
	// merge the results from memory and immutable memory
	// since the immutable memory is always older than memory, points after append is ordered
	e := c.store.entry(key)
	if c.snapshot != nil {
		snapshotEntries = c.snapshot.store.entry(key)
	}
	c.mu.RUnlock()

	if e == nil {
		if snapshotEntries == nil {
			// No values in hot cache or snapshots.
			return nil
		}
	} else {
		e.deduplicate()
	}

	// Build the sequence of entries that will be returned, in the correct order.
	// Calculate the required size of the destination buffer.
	var entries []*entry
	sz := 0

	if snapshotEntries != nil {
		snapshotEntries.deduplicate() // guarantee we are deduplicated
		entries = append(entries, snapshotEntries)
		sz += snapshotEntries.count()
	}

	if e != nil {
		entries = append(entries, e)
		sz += e.count()
	}

	// Any entries? If not, return.
	if sz == 0 {
		return nil
	}

	// Create the buffer, and copy all hot values and snapshots. Individual
	// entries are sorted at this point, so now the code has to check if the
	// resultant buffer will be sorted from start to finish.
	values := make(Values, sz)
	n := 0
	for _, e := range entries {
		e.mu.RLock()
		n += copy(values[n:], e.values)
		e.mu.RUnlock()
	}
	values = values[:n]
	values = values.Deduplicate()

	return values
}

// Delete removes all values for the given keys from the cache.
func (c *Cache) Delete(keys [][]byte) {
	c.DeleteRange(keys, math.MinInt64, math.MaxInt64)
}

// DeleteRange removes the values for all keys containing points
// with timestamps between between min and max from the cache.
//
// TODO(edd): Lock usage could possibly be optimised if necessary.
func (c *Cache) DeleteRange(keys [][]byte, min, max int64) {
	c.init()

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, k := range keys {
		// Make sure key exist in the cache, skip if it does not
		e := c.store.entry(k)
		if e == nil {
			continue
		}

		origSize := uint64(e.size())
		if min == math.MinInt64 && max == math.MaxInt64 {
			c.decreaseSize(origSize + uint64(len(k)))
			c.store.remove(k)
			continue
		}

		e.filter(min, max)
		if e.count() == 0 {
			c.store.remove(k)
			c.decreaseSize(origSize + uint64(len(k)))
			continue
		}

		c.decreaseSize(origSize - uint64(e.size()))
	}
	atomic.StoreInt64(&c.stats.MemSizeBytes, int64(c.Size()))
}

// SetMaxSize updates the memory limit of the cache.
func (c *Cache) SetMaxSize(size uint64) {
	c.mu.Lock()
	c.maxSize = size
	c.mu.Unlock()
}

// values returns the values for the key. It assumes the data is already sorted.
// It doesn't lock the cache but it does read-lock the entry if there is one for the key.
// values should only be used in compact.go in the CacheKeyIterator.
func (c *Cache) values(key []byte) Values {
	e := c.store.entry(key)
	if e == nil {
		return nil
	}
	e.mu.RLock()
	v := e.values
	e.mu.RUnlock()
	return v
}

// ApplyEntryFn applies the function f to each entry in the Cache.
// ApplyEntryFn calls f on each entry in turn, within the same goroutine.
// It is safe for use by multiple goroutines.
func (c *Cache) ApplyEntryFn(f func(key []byte, entry *entry) error) error {
	c.mu.RLock()
	store := c.store
	c.mu.RUnlock()
	return store.applySerial(f)
}

// CacheLoader processes a set of WAL segment files, and loads a cache with the data
// contained within those files.  Processing of the supplied files take place in the
// order they exist in the files slice.
type CacheLoader struct {
	files []string

	Logger *zap.Logger
}

// NewCacheLoader returns a new instance of a CacheLoader.
func NewCacheLoader(files []string) *CacheLoader {
	return &CacheLoader{
		files:  files,
		Logger: zap.NewNop(),
	}
}

// Load returns a cache loaded with the data contained within the segment files.
// If, during reading of a segment file, corruption is encountered, that segment
// file is truncated up to and including the last valid byte, and processing
// continues with the next segment file.
// recover cache from wal files
func (cl *CacheLoader) Load(cache *Cache) error {

	var r *WALSegmentReader
	for _, fn := range cl.files {
		if err := func() error {
			f, err := os.OpenFile(fn, os.O_CREATE|os.O_RDWR, 0666)
			if err != nil {
				return err
			}
			defer f.Close()

			// Log some information about the segments.
			stat, err := os.Stat(f.Name())
			if err != nil {
				return err
			}
			cl.Logger.Info("Reading file", zap.String("path", f.Name()), zap.Int64("size", stat.Size()))

			// Nothing to read, skip it
			if stat.Size() == 0 {
				return nil
			}

			if r == nil {
				r = NewWALSegmentReader(f)
				defer r.Close()
			} else {
				r.Reset(f)
			}

			for r.Next() {
				entry, err := r.Read()
				if err != nil {
					n := r.Count()
					cl.Logger.Info("File corrupt", zap.Error(err), zap.String("path", f.Name()), zap.Int64("pos", n))
					if err := f.Truncate(n); err != nil {
						return err
					}
					break
				}

				switch t := entry.(type) {
				case *WriteWALEntry:
					if err := cache.WriteMulti(t.Values); err != nil {
						return err
					}
				case *DeleteRangeWALEntry:
					cache.DeleteRange(t.Keys, t.Min, t.Max)
				case *DeleteWALEntry:
					cache.Delete(t.Keys)
				}
			}

			return r.Close()
		}(); err != nil {
			return err
		}
	}
	return nil
}

// WithLogger sets the logger on the CacheLoader.
func (cl *CacheLoader) WithLogger(log *zap.Logger) {
	cl.Logger = log.With(zap.String("service", "cacheloader"))
}

func (c *Cache) LastWriteTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastWriteTime
}

// UpdateAge updates the age statistic based on the current time.
func (c *Cache) UpdateAge() {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ageStat := int64(time.Since(c.lastSnapshot) / time.Millisecond)
	atomic.StoreInt64(&c.stats.CacheAgeMs, ageStat)
}

// UpdateCompactTime updates WAL compaction time statistic based on d.
func (c *Cache) UpdateCompactTime(d time.Duration) {
	atomic.AddInt64(&c.stats.WALCompactionTimeMs, int64(d/time.Millisecond))
}

// updateCachedBytes increases the cachedBytes counter by b.
func (c *Cache) updateCachedBytes(b uint64) {
	atomic.AddInt64(&c.stats.CachedBytes, int64(b))
}

// updateMemSize updates the memSize level by b.
func (c *Cache) updateMemSize(b int64) {
	atomic.AddInt64(&c.stats.MemSizeBytes, b)
}

func valueType(v Value) byte {
	switch v.(type) {
	case FloatValue:
		return 1
	case IntegerValue:
		return 2
	case StringValue:
		return 3
	case BooleanValue:
		return 4
	default:
		return 0
	}
}

// updateSnapshots updates the snapshotsCount and the diskSize levels.
func (c *Cache) updateSnapshots() {
	// Update disk stats
	atomic.StoreInt64(&c.stats.DiskSizeBytes, int64(atomic.LoadUint64(&c.snapshotSize)))
	atomic.StoreInt64(&c.stats.SnapshotCount, int64(c.snapshotAttempts))
}

type emptyStore struct{}

func (e emptyStore) entry(key []byte) *entry                        { return nil }
func (e emptyStore) write(key []byte, values Values) (bool, error)  { return false, nil }
func (e emptyStore) add(key []byte, entry *entry)                   {}
func (e emptyStore) remove(key []byte)                              {}
func (e emptyStore) keys(sorted bool) [][]byte                      { return nil }
func (e emptyStore) apply(f func([]byte, *entry) error) error       { return nil }
func (e emptyStore) applySerial(f func([]byte, *entry) error) error { return nil }
func (e emptyStore) reset()                                         {}
func (e emptyStore) split(n int) []storer                           { return nil }
func (e emptyStore) count() int                                     { return 0 }
