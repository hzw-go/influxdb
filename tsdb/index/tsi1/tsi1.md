## fork from [influxdb doc](https://docs.influxdata.com/influxdb/v1.8/concepts/tsi-details/)
### File organization

TSI (Time Series Index) is a log-structured merge tree-based database for InfluxDB series data.
TSI is composed of several parts:

* **Index**: Contains the entire index dataset for a single shard.

* **Partition**: Contains a sharded partition of the data for a shard.

* **LogFile**: Contains newly written series as an in-memory index and is persisted as a WAL.

* **IndexFile**: Contains an immutable, memory-mapped index built from a LogFile or merged from two contiguous index files.

There is also a **SeriesFile** which contains a set of all series keys across the entire database.
Each shard within the database shares the same series file.

### Writes

The following occurs when a write comes into the system:

1. Series is added to the series file or is looked up if it already exists. This returns an auto-incrementing series ID.
2. The series is sent to the Index. The index maintains a roaring bitmap of existing series IDs and ignores series that have already been created.
3. The series is hashed and sent to the appropriate Partition.
4. The Partition writes the series as an entry to the LogFile.
5. The LogFile writes the series to a write-ahead log file on disk and adds the series to a set of in-memory indexes.

### Compaction

Once the LogFile exceeds a threshold (5MB), then a new active log file is created and the previous one begins compacting into an IndexFile.
This first index file is at level 1 (L1).
The log file is considered level 0 (L0).

Index files can also be created by merging two smaller index files together.
For example, if contiguous two L1 index files exist then they can be merged into an L2 index file.

### Reads

The index provides several API calls for retrieving sets of data such as:

* `MeasurementIterator()`: Returns a sorted list of measurement names.
* `TagKeyIterator()`: Returns a sorted list of tag keys in a measurement.
* `TagValueIterator()`: Returns a sorted list of tag values for a tag key.
* `MeasurementSeriesIDIterator()`: Returns a sorted list of all series IDs for a measurement.
* `TagKeySeriesIDIterator()`: Returns a sorted list of all series IDs for a tag key.
* `TagValueSeriesIDIterator()`: Returns a sorted list of all series IDs for a tag value.

These iterators are all composable using several merge iterators.
For each type of iterator (measurement, tag key, tag value, series id), there are multiple merge iterator types:

* **Merge**: Deduplicates items from two iterators.
* **Intersect**: Returns only items that exist in two iterators.
* **Difference**: Only returns items from first iterator that don't exist in the second iterator.

For example, a query with a WHERE clause of `region != 'us-west'` that operates across two shards will construct a set of iterators like this:

```
DifferenceSeriesIDIterators(
    MergeSeriesIDIterators(
        Shard1.MeasurementSeriesIDIterator("m"),
        Shard2.MeasurementSeriesIDIterator("m"),
    ),
    MergeSeriesIDIterators(
        Shard1.TagValueSeriesIDIterator("m", "region", "us-west"),
        Shard2.TagValueSeriesIDIterator("m", "region", "us-west"),
    ),
)
```

### Log File Structure

The log file is simply structured as a list of LogEntry objects written to disk in sequential order. Log files are written until they reach 5MB and then they are compacted into index files.
The entry objects in the log can be of any of the following types:

* AddSeries
* DeleteSeries
* DeleteMeasurement
* DeleteTagKey
* DeleteTagValue

The in-memory index on the log file tracks the following:

* Measurements by name
* Tag keys by measurement
* Tag values by tag key
* Series by measurement
* Series by tag value
* Tombstones for series, measurements, tag keys, and tag values.

The log file also maintains bitsets for series ID existence and tombstones.
These bitsets are merged with other log files and index files to regenerate the full index bitset on startup.

### Index File Structure

The index file is an immutable file that tracks similar information to the log file, but all data is indexed and written to disk so that it can be directly accessed from a memory-map.

The index file has the following sections:

* **TagBlocks:** Maintains an index of tag values for a single tag key.
* **MeasurementBlock:** Maintains an index of measurements and their tag keys.
* **Trailer:** Stores offset information for the file as well as HyperLogLog sketches for cardinality estimation.

### Manifest

The MANIFEST file is stored in the index directory and lists all the files that belong to the index and the order in which they should be accessed.
This file is updated every time a compaction occurs.
Any files that are in the directory that are not in the index file are index files that are in the process of being compacted.

### FileSet

A file set is an in-memory snapshot of the manifest that is obtained while the InfluxDB process is running.
This is required to provide a consistent view of the index at a point-in-time.
The file set also facilitates reference counting for all of its files so that no file will be deleted via compaction until all readers of the file are done with it.


## related structs
./tsdb/tsi/
file_set
index
index_file
log_file
measurement_block
partition
tag_block

./tsdb/
series_file
series_index
series_partition
series_segment
series_set

### the function stack be called to check measurement exists
engine.MeasurementExists(tsdb/engine.go)
tsm.MeasurementExists(tsm1/engine.go)
index.MeasurementExists(tsi1/index.go:Index{})
partition.MeasurementExists(tsi1/partition.go:Partition{})
fs.Measurement(tsi1/file_set.go:FileSet{})
f.measurement(tsi1/index_file.go:IndexFile{} or tsi1/log_file.go:LogFile{})

As we can see, there is a relation between some structs above
```
Index
    Partition
        FileSet
            IndexFile | LogFile
```




At the end, i have to figure out what will happen during following scenarios
- [ ] insert point with new series
- [ ] insert point
- [ ] update point
- [ ] delete one point
- [ ] delete range 
- [ ] drop one series
- [ ] drop by tag
- [ ] drop by measurement
- [ ] drop shard
- [ ] show tag keys
- [ ] show tag values
- [ ] show series
- [ ] show measurements
```
[show measurements]
q: drop measurement only mark memory and append wal, how to mute the measurement in tsi file 
```