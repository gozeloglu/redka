package rzset

import (
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/nalgeon/redka/internal/core"
	"github.com/nalgeon/redka/internal/rkey"
	"github.com/nalgeon/redka/internal/sqlx"
)

const (
	sqlInter = `
	select elem, sum(score) as score
	from rzset
	  join rkey on key_id = rkey.id and (etime is null or etime > :now)
	where key in (:keys)
	group by elem
	having count(distinct key_id) = :nkeys
	order by sum(score), elem`

	sqlInterStore1 = `
	insert into rkey (key, type, version, mtime)
	values (:key, :type, :version, :mtime)
	returning id`

	sqlInterStore2 = `
	insert into rzset (key_id, elem, score)
	select :key_id, elem, sum(score) as score
	from rzset
	  join rkey on key_id = rkey.id and (etime is null or etime > :now)
	where key in (:keys)
	group by elem
	having count(distinct key_id) = :nkeys
	order by sum(score), elem`
)

// InterCmd intersects multiple sets.
type InterCmd struct {
	db        *DB
	tx        *Tx
	dest      string
	keys      []string
	aggregate string
}

// Dest sets the key to store the result of the intersection.
func (c InterCmd) Dest(dest string) InterCmd {
	c.dest = dest
	return c
}

// Sum changes the aggregation function to take the sum of scores.
func (c InterCmd) Sum() InterCmd {
	c.aggregate = sqlx.Sum
	return c
}

// Min changes the aggregation function to take the minimum score.
func (c InterCmd) Min() InterCmd {
	c.aggregate = sqlx.Min
	return c
}

// Max changes the aggregation function to take the maximum score.
func (c InterCmd) Max() InterCmd {
	c.aggregate = sqlx.Max
	return c
}

// Run returns the intersection of multiple sets.
// The intersection consists of elements that exist in all given sets.
// The score of each element is the aggregate of its scores in the given sets.
// If any of the source keys do not exist or are not sets, returns an empty slice.
func (c InterCmd) Run() ([]SetItem, error) {
	if c.db != nil {
		return c.inter(c.db.SQL)
	}
	if c.tx != nil {
		return c.inter(c.tx.tx)
	}
	return nil, nil
}

// Store intersects multiple sets and stores the result in a new set.
// Returns the number of elements in the resulting set.
// If the destination key already exists, it is fully overwritten
// (all old elements are removed and the new ones are inserted).
// If the destination key already exists and is not a set, returns ErrKeyType.
// If any of the source keys do not exist or are not sets, does nothing,
// except deleting the destination key if it exists.
func (c InterCmd) Store() (int, error) {
	if c.db != nil {
		var count int
		err := c.db.Update(func(tx *Tx) error {
			var err error
			count, err = c.store(tx.tx)
			return err
		})
		return count, err
	}
	if c.tx != nil {
		return c.store(c.tx.tx)
	}
	return 0, nil
}

// inter returns the intersection of multiple sets.
func (c InterCmd) inter(tx sqlx.Tx) ([]SetItem, error) {
	// Prepare query arguments.
	now := time.Now().UnixMilli()
	query := sqlInter
	if c.aggregate != sqlx.Sum {
		query = strings.Replace(query, sqlx.Sum, c.aggregate, 2)
	}
	query, keyArgs := sqlx.ExpandIn(query, ":keys", c.keys)
	args := slices.Concat([]any{now}, keyArgs, []any{len(c.keys)})

	// Execute the query.
	var rows *sql.Rows
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Build the resulting element-score slice.
	var items []SetItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	return items, nil
}

// store intersects multiple sets and stores the result in a new set.
func (c InterCmd) store(tx sqlx.Tx) (int, error) {
	// Delete the destination key if it exists.
	_, err := rkey.DeleteType(tx, core.TypeSortedSet, c.dest)
	if err != nil {
		return 0, err
	}

	// Insert the destination key and get its ID.
	now := time.Now().UnixMilli()
	args := []any{
		sql.Named("key", c.dest),
		sql.Named("type", core.TypeSortedSet),
		sql.Named("version", core.InitialVersion),
		sql.Named("mtime", now),
	}
	var keyID int
	err = tx.QueryRow(sqlInterStore1, args...).Scan(&keyID)
	if err != nil {
		return 0, sqlx.TypedError(err)
	}

	// Intersect the sets and store the result.
	query := sqlInterStore2
	if c.aggregate != sqlx.Sum {
		query = strings.Replace(query, sqlx.Sum, c.aggregate, 2)
	}
	query, keyArgs := sqlx.ExpandIn(query, ":keys", c.keys)
	args = slices.Concat([]any{keyID, now}, keyArgs, []any{len(c.keys)})

	res, err := tx.Exec(query, args...)
	if err != nil {
		return 0, err
	}

	// Return the number of elements in the resulting set.
	n, _ := res.RowsAffected()
	return int(n), nil
}
