package history

import (
	"encoding/binary"
	"errors"
	"slices"
)

// Packed blocks: one row holds one series' readings for one hour (1-minute and 5-minute tiers) or
// one day (15-minute tier). A presence bitmap marks the slots that have data (gaps cost a bit),
// then each column follows as zigzag-delta varints, so steady values take a byte or two.

// Tiers, by bucket width in minutes.
const (
	tier1  = 1  // raw 1-minute readings: cpu, mem
	tier5  = 5  // n, cpu median/min/max, mem median/min/max
	tier15 = 15 // same columns as tier5
)

func tierSpan(tier int) int64 { // seconds one block covers
	if tier == tier15 {
		return 86400
	}
	return 3600
}

func tierSlots(tier int) int { return int(tierSpan(tier) / int64(tier*60)) }

func tierCols(tier int) int {
	if tier == tier1 {
		return 2
	}
	return 7
}

// bucket is one slot of a block. For tier1, N is 1 and Med = Min = Max = the reading.
type bucket struct {
	N                   int64 // minutes of readings it covers
	CPU, CPUMin, CPUMax int64 // millicores
	Mem, MemMin, MemMax int64 // KiB
}

func (b bucket) cols(tier int) []int64 {
	if tier == tier1 {
		return []int64{b.CPU, b.Mem}
	}
	return []int64{b.N, b.CPU, b.CPUMin, b.CPUMax, b.Mem, b.MemMin, b.MemMax}
}

func bucketOf(tier int, c []int64) bucket {
	if tier == tier1 {
		return bucket{N: 1, CPU: c[0], CPUMin: c[0], CPUMax: c[0], Mem: c[1], MemMin: c[1], MemMax: c[1]}
	}
	return bucket{N: c[0], CPU: c[1], CPUMin: c[2], CPUMax: c[3], Mem: c[4], MemMin: c[5], MemMax: c[6]}
}

// block is a decoded row: slot index → bucket.
type block map[int]bucket

func encodeBlock(tier int, b block) []byte {
	slots := tierSlots(tier)
	out := make([]byte, (slots+7)/8)
	idx := make([]int, 0, len(b))
	for i := range b {
		if i >= 0 && i < slots {
			idx = append(idx, i)
		}
	}
	slices.Sort(idx)
	for _, i := range idx {
		out[i/8] |= 1 << (i % 8)
	}
	for c := range tierCols(tier) {
		var prev int64
		for _, i := range idx {
			v := b[i].cols(tier)[c]
			out = binary.AppendVarint(out, v-prev)
			prev = v
		}
	}
	return out
}

var errCorrupt = errors.New("usage history: corrupt block")

func decodeBlock(tier int, data []byte) (block, error) {
	slots := tierSlots(tier)
	nb := (slots + 7) / 8
	if len(data) < nb {
		return nil, errCorrupt
	}
	var idx []int
	for i := range slots {
		if data[i/8]&(1<<(i%8)) != 0 {
			idx = append(idx, i)
		}
	}
	data = data[nb:]
	cols := make([][]int64, tierCols(tier))
	for c := range cols {
		cols[c] = make([]int64, len(idx))
		var prev int64
		for j := range idx {
			d, k := binary.Varint(data)
			if k <= 0 {
				return nil, errCorrupt
			}
			data = data[k:]
			prev += d
			cols[c][j] = prev
		}
	}
	b := make(block, len(idx))
	row := make([]int64, len(cols))
	for j, i := range idx {
		for c := range cols {
			row[c] = cols[c][j]
		}
		b[i] = bucketOf(tier, row)
	}
	return b, nil
}

// median of integer readings: the middle one, or the mean of the two middle ones.
func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// rollRaw makes one bucket from 1-minute readings: exact median, min and max.
func rollRaw(raw []bucket) bucket {
	cpu, mem := make([]int64, len(raw)), make([]int64, len(raw))
	for i, r := range raw {
		cpu[i], mem[i] = r.CPU, r.Mem
	}
	return bucket{N: int64(len(raw)), CPU: median(cpu), CPUMin: slices.Min(cpu), CPUMax: slices.Max(cpu),
		Mem: median(mem), MemMin: slices.Min(mem), MemMax: slices.Max(mem)}
}
