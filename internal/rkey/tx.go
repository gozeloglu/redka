package rkey

import (
	"database/sql"
	"slices"
	"time"

	"github.com/nalgeon/redka/internal/core"
	"github.com/nalgeon/redka/internal/sqlx"
)

const sqlGet = `
select id, key, type, version, etime, mtime
from rkey
where key = ? and (etime is null or etime > ?)`

const sqlCount = `
select count(id) from rkey
where key in (:keys) and (etime is null or etime > :now)`

const sqlKeys = `
select id, key, type, version, etime, mtime from rkey
where key glob :pattern and (etime is null or etime > :now)`

const sqlScan = `
select id, key, type, version, etime, mtime from rkey
where id > :cursor and key glob :pattern and (etime is null or etime > :now)
limit :count`

const sqlRandom = `
select id, key, type, version, etime, mtime from rkey
where etime is null or etime > ?
order by random() limit 1`

const sqlExpire = `
update rkey set etime = :at
where key = :key and (etime is null or etime > :now)`

const sqlPersist = `
update rkey set etime = null
where key = :key and (etime is null or etime > :now)`

const sqlRename = `
update or replace rkey set
  id = old.id,
  key = :new_key,
  type = old.type,
  version = old.version+1,
  etime = old.etime,
  mtime = :now
from (
  select id, key, type, version, etime, mtime
  from rkey
  where key = :key and (etime is null or etime > :now)
) as old
where rkey.key = :key and (
  rkey.etime is null or rkey.etime > :now
)`

const sqlDelete = `
delete from rkey where key in (:keys)
  and (etime is null or etime > :now)`

const sqlDeleteType = `
delete from rkey where key in (:keys)
  and (etime is null or etime > :now)
  and type = :type`

const sqlDeleteAll = `
  delete from rkey;
  vacuum;
  pragma integrity_check;`

const sqlDeleteAllExpired = `
delete from rkey
where etime <= :now`

const sqlDeleteNExpired = `
delete from rkey
where rowid in (
  select rowid from rkey
  where etime <= :now
  limit :n
)`

const scanPageSize = 10

// Tx is a key repository transaction.
type Tx struct {
	tx sqlx.Tx
}

// NewTx creates a key repository transaction
// from a generic database transaction.
func NewTx(tx sqlx.Tx) *Tx {
	return &Tx{tx}
}

// Exists reports whether the key exists.
func (tx *Tx) Exists(key string) (bool, error) {
	count, err := Count(tx.tx, key)
	return count > 0, err
}

// Count returns the number of existing keys among specified.
func (tx *Tx) Count(keys ...string) (int, error) {
	return Count(tx.tx, keys...)
}

// Keys returns all keys matching pattern.
// Supports glob-style patterns like these:
//
//	key*  k?y  k[bce]y  k[!a-c][y-z]
//
// Use this method only if you are sure that the number of keys is
// limited. Otherwise, use the [Tx.Scan] or [Tx.Scanner] methods.
func (tx *Tx) Keys(pattern string) ([]core.Key, error) {
	now := time.Now().UnixMilli()
	args := []any{sql.Named("pattern", pattern), sql.Named("now", now)}
	scan := func(rows *sql.Rows) (core.Key, error) {
		var k core.Key
		err := rows.Scan(&k.ID, &k.Key, &k.Type, &k.Version, &k.ETime, &k.MTime)
		return k, err
	}
	var keys []core.Key
	keys, err := sqlx.Select(tx.tx, sqlKeys, args, scan)
	return keys, err
}

// Scan iterates over keys matching pattern.
// It returns the next pageSize keys based on the current state of the cursor.
// Returns an empty slice when there are no more keys.
// See [Tx.Keys] for pattern description.
// Set pageSize = 0 for default page size.
func (tx *Tx) Scan(cursor int, pattern string, pageSize int) (ScanResult, error) {
	now := time.Now().UnixMilli()
	if pageSize == 0 {
		pageSize = scanPageSize
	}
	args := []any{
		sql.Named("cursor", cursor),
		sql.Named("pattern", pattern),
		sql.Named("now", now),
		sql.Named("count", pageSize),
	}
	scan := func(rows *sql.Rows) (core.Key, error) {
		var k core.Key
		err := rows.Scan(&k.ID, &k.Key, &k.Type, &k.Version, &k.ETime, &k.MTime)
		return k, err
	}
	var keys []core.Key
	keys, err := sqlx.Select(tx.tx, sqlScan, args, scan)
	if err != nil {
		return ScanResult{}, err
	}

	// Select the maximum ID.
	maxID := 0
	for _, key := range keys {
		if key.ID > maxID {
			maxID = key.ID
		}
	}

	return ScanResult{maxID, keys}, nil
}

// Scanner returns an iterator for keys matching pattern.
// The scanner returns keys one by one, fetching keys from the
// database in pageSize batches when necessary.
// See [Tx.Keys] for pattern description.
// Set pageSize = 0 for default page size.
func (tx *Tx) Scanner(pattern string, pageSize int) *Scanner {
	return newScanner(tx, pattern, pageSize)
}

// Random returns a random key.
func (tx *Tx) Random() (core.Key, error) {
	now := time.Now().UnixMilli()
	var k core.Key
	err := tx.tx.QueryRow(sqlRandom, now).Scan(
		&k.ID, &k.Key, &k.Type, &k.Version, &k.ETime, &k.MTime,
	)
	if err == sql.ErrNoRows {
		return core.Key{}, nil
	}
	return k, err
}

// Get returns a specific key with all associated details.
func (tx *Tx) Get(key string) (core.Key, error) {
	return Get(tx.tx, key)
}

// Expire sets a time-to-live (ttl) for the key using a relative duration.
// After the ttl passes, the key is expired and no longer exists.
// Returns false is the key does not exist.
func (tx *Tx) Expire(key string, ttl time.Duration) (bool, error) {
	at := time.Now().Add(ttl)
	return tx.ExpireAt(key, at)
}

// ExpireAt sets an expiration time for the key. After this time,
// the key is expired and no longer exists.
// Returns false is the key does not exist.
func (tx *Tx) ExpireAt(key string, at time.Time) (bool, error) {
	now := time.Now().UnixMilli()
	args := []any{
		sql.Named("key", key),
		sql.Named("now", now),
		sql.Named("at", at.UnixMilli()),
	}
	res, err := tx.tx.Exec(sqlExpire, args...)
	if err != nil {
		return false, err
	}
	count, _ := res.RowsAffected()
	return count > 0, nil
}

// Persist removes the expiration time for the key.
// Returns false is the key does not exist.
func (tx *Tx) Persist(key string) (bool, error) {
	now := time.Now().UnixMilli()
	args := []any{sql.Named("key", key), sql.Named("now", now)}
	res, err := tx.tx.Exec(sqlPersist, args...)
	if err != nil {
		return false, err
	}
	count, _ := res.RowsAffected()
	return count > 0, nil
}

// Rename changes the key name.
// If there is an existing key with the new name, it is replaced.
func (tx *Tx) Rename(key, newKey string) error {
	// Make sure the old key exists.
	oldK, err := Get(tx.tx, key)
	if err != nil {
		return err
	}
	if !oldK.Exists() {
		return core.ErrNotFound
	}

	// If the keys are the same, do nothing.
	if key == newKey {
		return nil
	}

	// Delete the new key if it exists.
	_, err = tx.Delete(newKey)
	if err != nil {
		return err
	}

	// Rename the old key to the new key.
	now := time.Now().UnixMilli()
	args := []any{
		sql.Named("key", key),
		sql.Named("new_key", newKey),
		sql.Named("now", now),
	}
	_, err = tx.tx.Exec(sqlRename, args...)
	return err
}

// RenameNotExists changes the key name.
// If there is an existing key with the new name, does nothing.
// Returns true if the key was renamed, false otherwise.
func (tx *Tx) RenameNotExists(key, newKey string) (bool, error) {
	// Make sure the old key exists.
	oldK, err := Get(tx.tx, key)
	if err != nil {
		return false, err
	}
	if !oldK.Exists() {
		return false, core.ErrNotFound
	}

	// If the keys are the same, do nothing.
	if key == newKey {
		return false, nil
	}

	// Make sure the new key does not exist.
	exist, err := tx.Exists(newKey)
	if err != nil {
		return false, err
	}
	if exist {
		return false, nil
	}

	// Rename the old key to the new key.
	now := time.Now().UnixMilli()
	args := []any{
		sql.Named("key", key),
		sql.Named("new_key", newKey),
		sql.Named("now", now),
	}
	_, err = tx.tx.Exec(sqlRename, args...)
	return err == nil, err
}

// Delete deletes keys and their values, regardless of the type.
// Returns the number of deleted keys. Non-existing keys are ignored.
func (tx *Tx) Delete(keys ...string) (int, error) {
	return Delete(tx.tx, keys...)
}

// DeleteAll deletes all keys and their values, effectively resetting
// the database. Should not be run inside a database transaction.
func (tx *Tx) DeleteAll() error {
	_, err := tx.tx.Exec(sqlDeleteAll)
	return err
}

// deleteExpired deletes keys with expired TTL, but no more than n keys.
// If n = 0, deletes all expired keys.
func (tx *Tx) deleteExpired(n int) (int, error) {
	now := time.Now().UnixMilli()
	var res sql.Result
	var err error
	if n > 0 {
		args := []any{sql.Named("now", now), sql.Named("n", n)}
		res, err = tx.tx.Exec(sqlDeleteNExpired, args...)
	} else {
		res, err = tx.tx.Exec(sqlDeleteAllExpired, now)
	}
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	return int(count), err
}

// ScanResult represents a result of the Scan call.
type ScanResult struct {
	Cursor int
	Keys   []core.Key
}

// Scanner is the iterator for keys.
// Stops when there are no more keys or an error occurs.
type Scanner struct {
	db       *Tx
	cursor   int
	pattern  string
	pageSize int
	index    int
	cur      core.Key
	keys     []core.Key
	err      error
}

func newScanner(db *Tx, pattern string, pageSize int) *Scanner {
	if pageSize == 0 {
		pageSize = scanPageSize
	}
	return &Scanner{
		db:       db,
		cursor:   0,
		pattern:  pattern,
		pageSize: pageSize,
		index:    0,
		keys:     []core.Key{},
	}
}

// Scan advances to the next key, fetching keys from db as necessary.
// Returns false when there are no more keys or an error occurs.
func (sc *Scanner) Scan() bool {
	if sc.index >= len(sc.keys) {
		// Fetch a new page of keys.
		out, err := sc.db.Scan(sc.cursor, sc.pattern, sc.pageSize)
		if err != nil {
			sc.err = err
			return false
		}
		sc.cursor = out.Cursor
		sc.keys = out.Keys
		sc.index = 0
		if len(sc.keys) == 0 {
			return false
		}
	}
	// Advance to the next key from the current page.
	sc.cur = sc.keys[sc.index]
	sc.index++
	return true
}

// Key returns the current key.
func (sc *Scanner) Key() core.Key {
	return sc.cur
}

// Err returns the first error encountered during iteration.
func (sc *Scanner) Err() error {
	return sc.err
}

// Get returns the key data structure.
func Get(tx sqlx.Tx, key string) (core.Key, error) {
	now := time.Now().UnixMilli()
	var k core.Key
	err := tx.QueryRow(sqlGet, key, now).Scan(
		&k.ID, &k.Key, &k.Type, &k.Version, &k.ETime, &k.MTime,
	)
	if err == sql.ErrNoRows {
		return core.Key{}, nil
	}
	return k, err
}

// Count returns the number of existing keys among specified.
func Count(tx sqlx.Tx, keys ...string) (int, error) {
	now := time.Now().UnixMilli()
	query, keyArgs := sqlx.ExpandIn(sqlCount, ":keys", keys)
	args := slices.Concat(keyArgs, []any{sql.Named("now", now)})
	var count int
	err := tx.QueryRow(query, args...).Scan(&count)
	return count, err
}

// Delete deletes keys and their values (regardless of the type).
func Delete(tx sqlx.Tx, keys ...string) (int, error) {
	now := time.Now().UnixMilli()
	query, keyArgs := sqlx.ExpandIn(sqlDelete, ":keys", keys)
	args := slices.Concat(keyArgs, []any{sql.Named("now", now)})
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	affectedCount, _ := res.RowsAffected()
	return int(affectedCount), nil
}

// DeleteType deletes keys of a specific type.
// Returns the number of deleted keys.
// Non-existing keys and keys of other types are ignored.
func DeleteType(tx sqlx.Tx, typ core.TypeID, keys ...string) (int, error) {
	now := time.Now().UnixMilli()
	query, keyArgs := sqlx.ExpandIn(sqlDeleteType, ":keys", keys)
	args := slices.Concat(keyArgs, []any{sql.Named("now", now), sql.Named("type", typ)})
	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	affectedCount, _ := res.RowsAffected()
	return int(affectedCount), nil
}
