// Package hashsplit implements content-based splitting of byte streams.
package hashsplit

import (
	"bufio"
	"context"
	"io"
	"math/bits"

	"go4.org/rollsum"
)

const defaultSplitBits = 13

// Splitter hashsplits a byte sequence into chunks.
//
// Hashsplitting is a way of dividing a byte stream into pieces
// based on the stream's content rather than on any predetermined chunk size.
// As the Splitter reads the stream it maintains a rolling checksum of the last several bytes.
// A chunk boundary occurs when the rolling checksum has enough trailing bits set
// (where "enough" is a configurable setting that determines the average chunk size).
//
// Hashsplitting has benefits when it comes to representing multiple,
// slightly different versions of the same data.
// Consider, for example, the problem of adding EXIF tags to a JPEG image file.
// The tags appear near the beginning of the file, and the bulk of the image data follows.
// If the file were divided into chunks at (say) 8-kilobyte boundaries,
// then adding EXIF data near the beginning would alter every following chunk
// (except in the lucky case where the size of the added data is an exact multiple of 8kb).
// With hashsplitting, only the chunks in the vicinity of the change are affected.
//
// Hashsplitting is used to dramatically reduce storage and bandwidth requirements
// in projects like git, rsync, bup, and perkeep.
type Splitter struct {
	// Reset says whether to reset the rollsum state to zero at the beginning of each new chunk.
	// The default is false,
	// as in go4.org/rollsum,
	// but that means that a chunk's boundary is determined in part by the chunks that precede it.
	// You probably want to set this to true to make your chunks independent of each other,
	// unless you need go4.org/rollsum-compatible behavior.
	Reset bool

	// MinSize is the minimum chunk size. Only the final chunk may be smaller than this.
	// The default is zero, meaning chunks may be any size. (But they are never empty.)
	MinSize int

	// SplitBits is the number of trailing bits in the rolling checksum that must be set to produce a chunk.
	// The default (what you get if you leave it set to zero) is 13,
	// which means a chunk boundary occurs on average once every 8,192 bytes.
	//
	// (But thanks to math, that doesn't mean that 8,192 is the median chunk size.
	// The median chunk size is actually the logarithm, base (SplitBits-1)/SplitBits, of 0.5.
	// That makes the median chunk size 5,678 when SplitBits==13.)
	SplitBits uint

	rs *rollsum.RollSum
}

// Chunk is the output produced by Split.
// It contains a hashsplit sequence of the input bytes,
// the length of that sequence,
// and a "level."
//
// Why include the length in the chunk,
// when the caller could simply compute len(chunk.Bytes)?
// Because the caller might want to replace chunk.Bytes
// with a compact representation of them,
// such as a hash,
// while leaving Len alone.
// Tree,
// in turn,
// computes Offset and Size values from Chunk.Len,
// not len(chunk.Bytes).
//
// The level of a chunk is the number of additional trailing rollsum bits,
// beyond the number needed to make a chunk,
// that were set when the chunk was made.
// This number is used in constructing a hashsplit tree;
// see Tree.
type Chunk struct {
	Bytes      []byte
	Len, Level uint
}

// Split hashsplits its input using the default Splitter,
// calling a callback for each chunk produced.
//
// See Splitter.Split for more detail.
func Split(ctx context.Context, r io.Reader, f func(Chunk) error) error {
	s := &Splitter{SplitBits: defaultSplitBits}
	return s.Split(ctx, r, f)
}

// Split hashsplits its input.
//
// Bytes are read from r one at a time and added to the current chunk.
// The callback is invoked on the current chunk when s.SplitBits trailing bits of the rollsum state are set.
// The final chunk is sent regardless of the rollsum state, naturally.
//
// If the callback return an error, Split exits with that error.
func (s *Splitter) Split(ctx context.Context, r io.Reader, f func(Chunk) error) error {
	splitBits := s.SplitBits
	if splitBits == 0 {
		splitBits = defaultSplitBits
	}

	s.reset()

	var b []byte
	rr := bufio.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := rr.ReadByte()
		if err == io.EOF {
			if len(b) > 0 {
				tz, _ := s.checkSplit(splitBits)
				var extraBits uint
				if tz >= splitBits {
					extraBits = tz - splitBits
				}
				return f(Chunk{Bytes: b, Len: uint(len(b)), Level: extraBits})
			}
			return nil
		}
		if err != nil {
			return err
		}
		b = append(b, c)
		s.rs.Roll(c)
		if len(b) < s.MinSize {
			continue
		}
		if tz, shouldSplit := s.checkSplit(splitBits); shouldSplit {
			err = f(Chunk{Bytes: b, Len: uint(len(b)), Level: tz - splitBits})
			if err != nil {
				return err
			}
			b = []byte{}
			if s.Reset {
				s.reset()
			}
		}
	}
}

func (s *Splitter) reset() {
	s.rs = rollsum.New()
}

func (s *Splitter) checkSplit(splitBits uint) (uint, bool) {
	h := s.rs.Digest()
	tz := uint(bits.TrailingZeros32(h))
	return tz, tz >= splitBits
}

// Node is a node in the tree returned by Tree.
// A interior node ("level 1" and higher) contains one or more subnodes as children.
// A leaf node ("level 0") contains one or more byte slices,
// which are hashsplit chunks of the input.
// Exactly one of Nodes and Leaves is non-empty.
type Node struct {
	Nodes        []*Node
	Leaves       [][]byte
	Size, Offset uint64
}

// Tree assembles the output of Split into a hashsplit tree.
//
// A hashsplit tree provides another level of space-and-bandwidth savings
// over and above what Split gives you.
// Consider, again, the example of adding EXIF tags to a JPEG file.
// Although most chunks of the file will be the same before and after adding tags,
// the _list_ needed to reassemble those chunks into the original file will be very different:
// all the unaffected chunks must shift position to accommodate the new EXIF-containing chunks.
//
// A hashsplit tree organizes that list into a tree instead,
// with the property that only the tree nodes in the vicinity of the change will be affected.
// Most subtrees will remain the same.
//
// Chunks of hashsplit output are collected in a "level 0" node until
// one whose rolling checksum has levelBits extra bits set
// (beyond the number needed to complete a chunk).
// This adds the level-0 node as a child to a new level-1 node.
// If 2*levelBits extra bits are set,
// that adds the level-1 node to a new level-2 node,
// and so on.
//
// Return value is the root of the tree,
// pruned to remove any singleton nodes.
func Tree(inp <-chan Chunk) *Node {
	levels := []*Node{new(Node)}

	for chunk := range inp {
		levels[0].Leaves = append(levels[0].Leaves, chunk.Bytes)
		for _, n := range levels {
			n.Size += uint64(chunk.Len)
		}
		for i := uint(0); i < chunk.Level; i++ {
			if i == uint(len(levels))-1 {
				levels = append(levels, &Node{
					Size: levels[i].Size,
				})
			}
			levels[i+1].Nodes = append(levels[i+1].Nodes, levels[i])
			levels[i] = &Node{
				Offset: levels[i+1].Offset + levels[i+1].Size,
			}
		}
	}
	if len(levels[0].Leaves) > 0 {
		for i := 0; i < len(levels)-1; i++ {
			levels[i+1].Nodes = append(levels[i+1].Nodes, levels[i])
		}
	}

	root := levels[len(levels)-1]
	for len(root.Nodes) == 1 {
		root = root.Nodes[0]
	}

	return root
}
