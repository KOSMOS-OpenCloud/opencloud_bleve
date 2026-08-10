package scorch

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/RoaringBitmap/roaring/v2"
	segment "github.com/blevesearch/scorch_segment_api/v2"
)

// debugLevel controls the verification intensity.
//   "off"   = no verification (default)
//   "smart" = verify only merge results + count bitmap cardinality before/after
//   "full"  = verify all segments after every introduction
var debugLevel string

func init() {
	debugLevel = os.Getenv("SCORCH_DEBUG")
	if debugLevel == "" || debugLevel == "false" || debugLevel == "off" {
		debugLevel = "off"
	}
	// backwards compat: "true" = "full" (old SCORCH_HEAVY_DEBUG)
	if debugLevel == "true" {
		debugLevel = "full"
	}
	if debugLevel != "off" {
		log.Printf("scorch: DEBUG level=%s", debugLevel)
	}
}

func isDebugSmart() bool { return debugLevel == "smart" || debugLevel == "full" }
func isDebugFull() bool  { return debugLevel == "full" }

// verifySegment reads every posting of every term of every field in a single
// segment. Returns nil if readable, or the first error found.
func verifySegment(seg segment.Segment, deleted *roaring.Bitmap, label string) error {
	fields := seg.Fields()
	for _, field := range fields {
		dict, err := seg.Dictionary(field)
		if err != nil {
			return fmt.Errorf("[%s] field=%s: Dictionary(): %v", label, field, err)
		}
		dictItr := dict.AutomatonIterator(nil, nil, nil)
		for {
			entry, err := dictItr.Next()
			if err != nil {
				return fmt.Errorf("[%s] field=%s: dictItr: %v", label, field, err)
			}
			if entry == nil {
				break
			}
			pl, err := dict.PostingsList([]byte(entry.Term), deleted, nil)
			if err != nil {
				return fmt.Errorf("[%s] field=%s term=%q: PostingsList: %v",
					label, field, truncTerm(entry.Term), err)
			}
			itr := pl.Iterator(true, true, true, nil)
			count := 0
			for {
				p, err := itr.Next()
				if err != nil {
					return fmt.Errorf("[%s] field=%s term=%q posting=%d: Iterator: %v",
						label, field, truncTerm(entry.Term), count, err)
				}
				if p == nil {
					break
				}
				count++
			}
		}
	}
	return nil
}

// verifySnapshot reads every posting of every term of every field in every
// segment of the snapshot. Logs exact location of any error.
func (s *Scorch) verifySnapshot(snapshot *IndexSnapshot, trigger string) {
	if !isDebugFull() {
		return
	}

	start := time.Now()
	var totalSegments, totalFields, totalTerms, totalPostings int
	var errors int

	for segIdx, segSnap := range snapshot.segment {
		fields := segSnap.segment.Fields()
		if len(fields) == 0 {
			log.Printf("scorch: VERIFY WARN [%s] seg=%d: no fields", trigger, segIdx)
			continue
		}
		totalSegments++

		for _, field := range fields {
			totalFields++
			dict, err := segSnap.segment.Dictionary(field)
			if err != nil {
				log.Printf("scorch: VERIFY ERROR [%s] seg=%d field=%s: Dictionary(): %v", trigger, segIdx, field, err)
				errors++
				continue
			}

			dictItr := dict.AutomatonIterator(nil, nil, nil)
			for {
				entry, err := dictItr.Next()
				if err != nil {
					log.Printf("scorch: VERIFY ERROR [%s] seg=%d field=%s: dictItr.Next(): %v", trigger, segIdx, field, err)
					errors++
					break
				}
				if entry == nil {
					break
				}
				totalTerms++

				term := entry.Term
				pl, err := dict.PostingsList([]byte(term), segSnap.deleted, nil)
				if err != nil {
					log.Printf("scorch: VERIFY ERROR [%s] seg=%d field=%s term=%q: PostingsList(): %v",
						trigger, segIdx, field, truncTerm(term), err)
					errors++
					continue
				}

				itr := pl.Iterator(true, true, true, nil)
				postingCount := 0
				for {
					p, err := itr.Next()
					if err != nil {
						log.Printf("scorch: VERIFY ERROR [%s] seg=%d field=%s term=%q posting=%d: Iterator.Next(): %v",
							trigger, segIdx, field, truncTerm(term), postingCount, err)
						errors++
						break
					}
					if p == nil {
						break
					}
					postingCount++
					totalPostings++
				}
			}
		}
	}

	dur := time.Since(start)
	if errors > 0 {
		log.Printf("scorch: VERIFY FAILED [%s] %d errors in %d segments, %d fields, %d terms, %d postings (%v)",
			trigger, errors, totalSegments, totalFields, totalTerms, totalPostings, dur)
	} else if dur > 5*time.Second {
		log.Printf("scorch: VERIFY OK [%s] %d segments, %d fields, %d terms, %d postings (%v)",
			trigger, totalSegments, totalFields, totalTerms, totalPostings, dur)
	}
}

func truncTerm(t string) string {
	if len(t) > 80 {
		return t[:80] + "..."
	}
	return t
}

// verifyCurrentRoot verifies the current root snapshot (acquires rootLock briefly).
func (s *Scorch) verifyCurrentRoot(trigger string) {
	if !isDebugFull() {
		return
	}

	s.rootLock.RLock()
	root := s.root
	if root != nil {
		root.AddRef()
	}
	s.rootLock.RUnlock()

	if root == nil {
		return
	}
	defer func() { _ = root.DecRef() }()

	s.verifySnapshot(root, trigger)
}

// cloneDropBitmaps returns deep copies of the drop bitmaps to prevent
// mutation during merge. This is the candidate fix for MB-70770.
func cloneDropBitmaps(drops []*roaring.Bitmap) []*roaring.Bitmap {
	cloned := make([]*roaring.Bitmap, len(drops))
	for i, bm := range drops {
		if bm != nil {
			cloned[i] = bm.Clone()
		}
	}
	return cloned
}

// checkDropBitmapStability compares drop bitmap cardinalities before and after
// a merge. If any changed, the bitmap was mutated during the merge — confirming
// the race condition.
func checkDropBitmapStability(label string, segIDs []uint64, before, after []uint64) {
	for i := range before {
		if before[i] != after[i] {
			log.Printf("scorch: DROP BITMAP CHANGED [%s] segment=%d: before=%d after=%d (delta=%d)",
				label, segIDs[i], before[i], after[i], int64(after[i])-int64(before[i]))
		}
	}
}

// captureDropCardinalities records the cardinality of each drop bitmap.
func captureDropCardinalities(drops []*roaring.Bitmap) []uint64 {
	cards := make([]uint64, len(drops))
	for i, bm := range drops {
		if bm != nil {
			cards[i] = bm.GetCardinality()
		}
	}
	return cards
}

// VerifyIndex is a public API to verify the current index state.
func (s *Scorch) VerifyIndex() error {
	s.rootLock.RLock()
	root := s.root
	if root != nil {
		root.AddRef()
	}
	s.rootLock.RUnlock()

	if root == nil {
		return fmt.Errorf("no root snapshot")
	}
	defer func() { _ = root.DecRef() }()

	for segIdx, segSnap := range root.segment {
		if err := verifySegment(segSnap.segment, segSnap.deleted, fmt.Sprintf("seg%d", segIdx)); err != nil {
			return err
		}
	}
	return nil
}
