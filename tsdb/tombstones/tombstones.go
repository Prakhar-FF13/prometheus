// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tombstones

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/encoding"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/prometheus/prometheus/tsdb/fileutil"
)

const TombstonesFilename = "tombstones"

const (
	// MagicTombstone is 4 bytes at the head of a tombstone file.
	MagicTombstone = 0x0130BA30

	tombstoneFormatV1          = 1
	tombstoneFormatVersionSize = 1
	tombstonesHeaderSize       = 5
	tombstonesCRCSize          = 4
)

// The table gets initialized with sync.Once but may still cause a race
// with any other use of the crc32 package anywhere. Thus we initialize it
// before.
var castagnoliTable *crc32.Table

func init() {
	castagnoliTable = crc32.MakeTable(crc32.Castagnoli)
}

// newCRC32 initializes a CRC32 hash with a preconfigured polynomial, so the
// polynomial may be easily changed in one location at a later time, if necessary.
func newCRC32() hash.Hash32 {
	return crc32.New(castagnoliTable)
}

// Reader gives access to tombstone intervals by series reference.
type Reader interface {
	// Get returns deletion intervals for the series with the given reference.
	Get(ref storage.SeriesRef) (Intervals, error)

	// Iter calls the given function for each encountered interval.
	Iter(func(storage.SeriesRef, Intervals) error) error

	// Total returns the total count of tombstones.
	Total() uint64

	// Close any underlying resources
	Close() error
}

func WriteFile(logger *slog.Logger, dir string, tr Reader) (int64, error) {
	path := filepath.Join(dir, TombstonesFilename)
	tmp := path + ".tmp"
	hash := newCRC32()
	var size int

	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer func() {
		if f != nil {
			if err := f.Close(); err != nil {
				logger.Error("close tmp file", "err", err.Error())
			}
		}
		if err := os.RemoveAll(tmp); err != nil {
			logger.Error("remove tmp file", "err", err.Error())
		}
	}()

	buf := encoding.Encbuf{B: make([]byte, 3*binary.MaxVarintLen64)}
	buf.Reset()
	// Write the meta.
	buf.PutBE32(MagicTombstone)
	n, err := f.Write(buf.Get())
	if err != nil {
		return 0, err
	}
	size += n

	bytes, err := Encode(tr)
	if err != nil {
		return 0, fmt.Errorf("encoding tombstones: %w", err)
	}

	// Ignore first byte which is the format type. We do this for compatibility.
	if _, err := hash.Write(bytes[tombstoneFormatVersionSize:]); err != nil {
		return 0, fmt.Errorf("calculating hash for tombstones: %w", err)
	}

	n, err = f.Write(bytes)
	if err != nil {
		return 0, fmt.Errorf("writing tombstones: %w", err)
	}
	size += n

	n, err = f.Write(hash.Sum(nil))
	if err != nil {
		return 0, err
	}
	size += n

	if err := f.Sync(); err != nil {
		return 0, tsdb_errors.NewMulti(err, f.Close()).Err()
	}

	if err = f.Close(); err != nil {
		return 0, err
	}
	f = nil
	return int64(size), fileutil.Replace(tmp, path)
}

// Encode encodes the tombstones from the reader.
// It does not attach any magic number or checksum.
func Encode(tr Reader) ([]byte, error) {
	buf := encoding.Encbuf{}
	buf.PutByte(tombstoneFormatV1)
	err := tr.Iter(func(ref storage.SeriesRef, ivs Intervals) error {
		for _, iv := range ivs {
			buf.PutUvarint64(uint64(ref))
			buf.PutVarint64(iv.Mint)
			buf.PutVarint64(iv.Maxt)
		}
		return nil
	})
	return buf.Get(), err
}

// Decode decodes the tombstones from the bytes
// which was encoded using the Encode method.
func Decode(b []byte) (Reader, error) {
	d := &encoding.Decbuf{B: b}
	if flag := d.Byte(); flag != tombstoneFormatV1 {
		return nil, fmt.Errorf("invalid tombstone format %x", flag)
	}

	if d.Err() != nil {
		return nil, d.Err()
	}

	stonesMap := NewMemTombstones()
	for d.Len() > 0 {
		k := storage.SeriesRef(d.Uvarint64())
		mint := d.Varint64()
		maxt := d.Varint64()
		if d.Err() != nil {
			return nil, d.Err()
		}

		stonesMap.AddInterval(k, Interval{mint, maxt})
	}
	return stonesMap, nil
}

// Stone holds the information on the posting and time-range
// that is deleted.
type Stone struct {
	Ref       storage.SeriesRef
	Intervals Intervals
}

func ReadTombstones(dir string) (Reader, int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, TombstonesFilename))
	switch {
	case os.IsNotExist(err):
		return NewMemTombstones(), 0, nil
	case err != nil:
		return nil, 0, err
	}

	if len(b) < tombstonesHeaderSize {
		return nil, 0, fmt.Errorf("tombstones header: %w", encoding.ErrInvalidSize)
	}

	d := &encoding.Decbuf{B: b[:len(b)-tombstonesCRCSize]}
	if mg := d.Be32(); mg != MagicTombstone {
		return nil, 0, fmt.Errorf("invalid magic number %x", mg)
	}

	// Verify checksum.
	hash := newCRC32()
	// Ignore first byte which is the format type.
	if _, err := hash.Write(d.Get()[tombstoneFormatVersionSize:]); err != nil {
		return nil, 0, fmt.Errorf("write to hash: %w", err)
	}
	if binary.BigEndian.Uint32(b[len(b)-tombstonesCRCSize:]) != hash.Sum32() {
		return nil, 0, errors.New("checksum did not match")
	}

	if d.Err() != nil {
		return nil, 0, d.Err()
	}

	stonesMap, err := Decode(d.Get())
	if err != nil {
		return nil, 0, err
	}

	return stonesMap, int64(len(b)), nil
}

type MemTombstones struct {
	intvlGroups map[storage.SeriesRef]Intervals
	mtx         sync.RWMutex
}

// NewMemTombstones creates new in memory Tombstone Reader
// that allows adding new intervals.
func NewMemTombstones() *MemTombstones {
	return &MemTombstones{intvlGroups: make(map[storage.SeriesRef]Intervals)}
}

func NewTestMemTombstones(intervals []Intervals) *MemTombstones {
	ret := NewMemTombstones()
	for i, intervalsGroup := range intervals {
		for _, interval := range intervalsGroup {
			ret.AddInterval(storage.SeriesRef(i+1), interval)
		}
	}
	return ret
}

func (t *MemTombstones) Get(ref storage.SeriesRef) (Intervals, error) {
	t.mtx.RLock()
	defer t.mtx.RUnlock()
	intervals, ok := t.intvlGroups[ref]
	if !ok {
		return nil, nil
	}
	// Make a copy to avoid race.
	res := make(Intervals, len(intervals))
	copy(res, intervals)
	return res, nil
}

func (t *MemTombstones) DeleteTombstones(refs map[storage.SeriesRef]struct{}) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	for ref := range refs {
		delete(t.intvlGroups, ref)
	}
}

func (t *MemTombstones) TruncateBefore(beforeT int64) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	for ref, ivs := range t.intvlGroups {
		i := len(ivs) - 1
		for ; i >= 0; i-- {
			if beforeT > ivs[i].Maxt {
				break
			}
		}
		if len(ivs[i+1:]) == 0 {
			delete(t.intvlGroups, ref)
		} else {
			newIvs := make(Intervals, len(ivs[i+1:]))
			copy(newIvs, ivs[i+1:])
			t.intvlGroups[ref] = newIvs
		}
	}
}

func (t *MemTombstones) Iter(f func(storage.SeriesRef, Intervals) error) error {
	t.mtx.RLock()
	defer t.mtx.RUnlock()
	for ref, ivs := range t.intvlGroups {
		if err := f(ref, ivs); err != nil {
			return err
		}
	}
	return nil
}

func (t *MemTombstones) Total() uint64 {
	t.mtx.RLock()
	defer t.mtx.RUnlock()

	total := uint64(0)
	for _, ivs := range t.intvlGroups {
		total += uint64(len(ivs))
	}
	return total
}

// AddInterval to an existing memTombstones.
func (t *MemTombstones) AddInterval(ref storage.SeriesRef, itvs ...Interval) {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	for _, itv := range itvs {
		t.intvlGroups[ref] = t.intvlGroups[ref].Add(itv)
	}
}

func (*MemTombstones) Close() error {
	return nil
}

// Interval represents a single time-interval.
type Interval struct {
	Mint, Maxt int64
}

func (tr Interval) InBounds(t int64) bool {
	return t >= tr.Mint && t <= tr.Maxt
}

func (tr Interval) IsSubrange(dranges Intervals) bool {
	for _, r := range dranges {
		if r.InBounds(tr.Mint) && r.InBounds(tr.Maxt) {
			return true
		}
	}

	return false
}

// Intervals represents	a set of increasing and non-overlapping time-intervals.
type Intervals []Interval

// Add the new time-range to the existing ones.
// The existing ones must be sorted.
func (in Intervals) Add(n Interval) Intervals {
	if len(in) == 0 {
		return append(in, n)
	}
	// Find min and max indexes of intervals that overlap with the new interval.
	// Intervals are closed [t1, t2] and t is discreet, so if neighbour intervals are 1 step difference
	// to the new one, we can merge those together.
	mini := 0
	if n.Mint != math.MinInt64 { // Avoid overflow.
		mini = sort.Search(len(in), func(i int) bool { return in[i].Maxt >= n.Mint-1 })
		if mini == len(in) {
			return append(in, n)
		}
	}

	maxi := len(in)
	if n.Maxt != math.MaxInt64 { // Avoid overflow.
		maxi = sort.Search(len(in)-mini, func(i int) bool { return in[mini+i].Mint > n.Maxt+1 })
		if maxi == 0 {
			if mini == 0 {
				return append(Intervals{n}, in...)
			}
			return append(in[:mini], append(Intervals{n}, in[mini:]...)...)
		}
	}

	if n.Mint < in[mini].Mint {
		in[mini].Mint = n.Mint
	}
	in[mini].Maxt = max(n.Maxt, in[maxi+mini-1].Maxt)
	return append(in[:mini+1], in[maxi+mini:]...)
}
