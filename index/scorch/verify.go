package scorch

import (
	"fmt"
	"log"
	"os"
	"time"
)

func init() {
	if os.Getenv("SCORCH_HEAVY_DEBUG") == "true" {
		scorchHeavyDebug = true
		log.Printf("scorch: HEAVY_DEBUG enabled — verifying all segments after every introduction")
	}
}

var scorchHeavyDebug bool

// verifySnapshot reads every posting of every term of every field in every
// segment of the snapshot. If any ReadUvarint or other error occurs, it logs
// the exact location (segment, field, term, posting count) and returns the error.
// This is extremely expensive and should only be enabled for debugging.
func (s *Scorch) verifySnapshot(snapshot *IndexSnapshot, trigger string) {
	if !scorchHeavyDebug && !s.heavyDebug {
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
			// dictItr has no Close()
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
	if !scorchHeavyDebug && !s.heavyDebug {
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

// VerifyIndex is a public API to verify the current index state.
// Returns nil if all segments are readable, or an error describing the first problem.
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
		fields := segSnap.segment.Fields()
		if len(fields) == 0 {
			continue
		}
		for _, field := range fields {
			dict, err := segSnap.segment.Dictionary(field)
			if err != nil {
				return fmt.Errorf("seg %d field %s: Dictionary(): %v", segIdx, field, err)
			}
			dictItr := dict.AutomatonIterator(nil, nil, nil)
			for {
				entry, err := dictItr.Next()
				if err != nil {
					// dictItr has no Close()
					return fmt.Errorf("seg %d field %s: dictItr: %v", segIdx, field, err)
				}
				if entry == nil {
					break
				}
				pl, err := dict.PostingsList([]byte(entry.Term), segSnap.deleted, nil)
				if err != nil {
					// dictItr has no Close()
					return fmt.Errorf("seg %d field %s term %q: PostingsList: %v", segIdx, field, truncTerm(entry.Term), err)
				}
				itr := pl.Iterator(true, true, true, nil)
				for {
					p, err := itr.Next()
					if err != nil {
						// dictItr has no Close()
						return fmt.Errorf("seg %d field %s term %q: Iterator: %v", segIdx, field, truncTerm(entry.Term), err)
					}
					if p == nil {
						break
					}
				}
			}
			// dictItr has no Close()
		}
	}
	return nil
}
