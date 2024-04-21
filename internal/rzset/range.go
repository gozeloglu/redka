package rzset

import (
	"database/sql"
	"strings"
	"time"

	"github.com/nalgeon/redka/internal/sqlx"
)

const (
	sqlRangeRank = `
	with ranked as (
	select elem, score, (row_number() over w - 1) as rank
	from rzset
	  join rkey on key_id = rkey.id and (etime is null or etime > :now)
	where key = :key
	window w as (partition by key_id order by score asc, elem asc)
	)
	select elem, score
	from ranked
	where rank between :start and :stop
	order by rank asc, elem asc`

	sqlRangeScore = `
	select elem, score
	from rzset
	  join rkey on key_id = rkey.id and (etime is null or etime > :now)
	where key = :key
	and score between :start and :stop
	order by score asc, elem asc`
)

type byRank struct {
	start, stop int
}

type byScore struct {
	start, stop float64
}

// RangeCmd retrieves a range of elements from a sorted set.
type RangeCmd struct {
	tx      sqlx.Tx
	key     string
	byRank  *byRank
	byScore *byScore
	sortDir string
	offset  int
	count   int
}

// ByRank sets filtering by rank.
func (c RangeCmd) ByRank(start, stop int) RangeCmd {
	c.byRank = &byRank{start, stop}
	c.byScore = nil
	return c
}

// ByScore sets filtering by score.
func (c RangeCmd) ByScore(start, stop float64) RangeCmd {
	c.byScore = &byScore{start, stop}
	c.byRank = nil
	return c
}

// Asc sets the sorting direction to ascending.
func (c RangeCmd) Asc() RangeCmd {
	c.sortDir = sqlx.Asc
	return c
}

// Desc sets the sorting direction to descending.
func (c RangeCmd) Desc() RangeCmd {
	c.sortDir = sqlx.Desc
	return c
}

// Offset sets the offset of the range.
// Only takes effect when filtering by score.
func (c RangeCmd) Offset(offset int) RangeCmd {
	c.offset = offset
	return c
}

// Count sets the maximum number of elements to return.
// Only takes effect when filtering by score.
func (c RangeCmd) Count(count int) RangeCmd {
	c.count = count
	return c
}

// Run returns a range of elements from a sorted set.
// Uses either by-rank or by-score filtering. The range is inclusive
// of both start and stop. The elements are sorted by score and then
// by element according to the sorting direction.
//
// Offset and count are optional, and only take effect
// when filtering by score.
//
// If the key does not exist or is not a sorted set,
// returns a nil slice.
func (c RangeCmd) Run() ([]SetItem, error) {
	if c.byRank != nil {
		return c.rangeRank()
	}
	if c.byScore != nil {
		return c.rangeScore()
	}
	return nil, nil
}

// rangeRank retrieves a range of elements by rank.
func (c RangeCmd) rangeRank() ([]SetItem, error) {
	// Check start and stop values.
	if c.byRank.start < 0 || c.byRank.stop < 0 {
		return nil, nil
	}

	// Change sort direction if necessary.
	query := sqlRangeRank
	if c.sortDir != sqlx.Asc {
		query = strings.Replace(query, sqlx.Asc, c.sortDir, -1)
	}

	// Prepare query arguments.
	args := []any{
		sql.Named("key", c.key),
		sql.Named("now", time.Now().UnixMilli()),
		sql.Named("start", c.byRank.start),
		sql.Named("stop", c.byRank.stop),
	}

	// Execute the query.
	rows, err := c.tx.Query(query, args...)
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

// rangeScore retrieves a range of elements by score.
func (c RangeCmd) rangeScore() ([]SetItem, error) {
	// Change sort direction if necessary.
	query := sqlRangeScore
	if c.sortDir != sqlx.Asc {
		query = strings.Replace(query, sqlx.Asc, c.sortDir, -1)
	}

	// Add offset and count if necessary.
	if c.offset > 0 && c.count > 0 {
		query += " limit :offset, :count"
	} else if c.count > 0 {
		query += " limit :count"
	} else if c.offset > 0 {
		query += " limit :offset, -1"
	}

	// Prepare query arguments.
	args := []any{
		sql.Named("key", c.key),
		sql.Named("now", time.Now().UnixMilli()),
		sql.Named("start", c.byScore.start),
		sql.Named("stop", c.byScore.stop),
		sql.Named("offset", c.offset),
		sql.Named("count", c.count),
	}

	// Execute the query.
	rows, err := c.tx.Query(query, args...)
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
