# DO NOT USE: Pebble is an incomplete work-in-progress.

# Pebble

Pebble is a LevelDB/RocksDB inspired key-value store focused on
performance and internal usage by CockroachDB. Pebble inherits the
RocksDB file formats and a few extensions such as range deletion
tombstones, table-level bloom filters, and updates to the MANIFEST
format.

Pebble intentionally does not aspire to include every feature in
RocksDB and is specifically targetting the use case and feature set
needed by CockroachDB:

* Block-based tables
* Indexed batches
* [[TODO]](https://github.com/petermattis/pebble/issues/6) Iterator options (prefix, lower/upper bound, table filter)
* Level-based compaction
* Merge operator
* [[TODO]](https://github.com/petermattis/pebble/issues/5) Prefix bloom filters
* [[TODO]](https://github.com/petermattis/pebble/issues/1) Range deletion tombstones
* Reverse iteration
* SSTable ingestion
* Table-level bloom filters

RocksDB has a large number of features that are not implemented in
Pebble:

* Backups and checkpoints
* Column families
* Delete files in range
* FIFO compaction style
* Forward iterator / tailing iterator
* Hash table format
* Memtable bloom filter
* Persistent cache
* Pin iterator key / value
* Plain table format
* Single delete
* Snapshots
* SSTable ingest-behind
* Transactions
* Universal compaction style

Pebble may silently corrupt data or behave incorrectly if used with a
RocksDB database that uses a feature Pebble doesn't support. Caveat
emptor!

## Pedigree

Pebble is based on the incomplete Go version of LevelDB:

https://github.com/golang/leveldb

The Go version of LevelDB is based on the C++ original:

https://github.com/google/leveldb

Optimizations and inspiration were drawn from RocksDB:

https://github.com/facebook/rocksdb
