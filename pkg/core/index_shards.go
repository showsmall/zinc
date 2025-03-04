/* Copyright 2022 Zinc Labs Inc. and Contributors
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
 */

package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blugelabs/bluge"
	"github.com/blugelabs/bluge/analysis"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"github.com/zinclabs/zincsearch/pkg/config"
	"github.com/zinclabs/zincsearch/pkg/errors"
	"github.com/zinclabs/zincsearch/pkg/meta"
	"github.com/zinclabs/zincsearch/pkg/uquery/source"
	"github.com/zinclabs/zincsearch/pkg/wal"
)

const (
	ShardIDNeedLatest int64 = -1 // get lastest shardID
	ShardIDNeedUpdate int64 = -2 // get all shardIDs
)

// IndexShard first layer shard by fixed number shards for index.
// Use hash algorithm distribute documents to different shards.
// This shards let we can concurrency write to many shards in same index.
// The shards num can not be modify, because if change the num
// hash algorithm will distribute the same docID to another shard,
// then we will can not found the old document, maybe cause duplicate documents.
// First layer shard just used for distribute not really store documents.
type IndexShard struct {
	open   uint64
	name   string // shard name: index/shardID
	root   *Index
	ref    *meta.IndexShard
	shards []*IndexSecondShard
	wal    *wal.Log
	lock   sync.RWMutex
	close  chan struct{}
}

// IndexSecondShard second layer shard by auto increate shards for index.
// Under first layer shards, Documents will store in this layer shards.
// Use a environment `config.ZINC_SHARD_MAX_SIZE` to control second layer shard max size.
// If the shard size over limit then will auto create a new shard for accept new documents.
// And we will log time range for this layer shards, when query data we can use time range
// filter which shards need to find data. We keep one shard size wouldn't over limit,
// we will fozen old shards, just write new documents to new shards and do merge in new shards
// this will improve shard performance.
type IndexSecondShard struct {
	root   *Index
	ref    *meta.IndexSecondShard
	writer *bluge.Writer
	lock   sync.RWMutex
}

// GetShardByDocID return the shard by hash docID
func (index *Index) GetShardByDocID(docID string) *IndexShard {
	shardKey := index.shardHashing.Lookup(docID)
	return index.shards[shardKey]
}

// CheckShards check all shards status if need create new second layer shard
func (index *Index) CheckShards() error {
	for _, shard := range index.shards {
		if err := shard.CheckShards(); err != nil {
			return err
		}
	}
	return nil
}

// CheckShards check current shard is reach the maximum shard size or create a new shard
func (s *IndexShard) CheckShards() error {
	w, err := s.GetWriter()
	if err != nil {
		return err
	}
	_, size := w.DirectoryStats()
	if size > config.Global.Shard.MaxSize {
		return s.NewShard()
	}
	return nil
}

func (s *IndexShard) GetIndexName() string {
	return s.root.GetName()
}

func (s *IndexShard) GetShardName() string {
	return s.name
}

func (s *IndexShard) GetID() string {
	return s.ref.ID
}

func (s *IndexShard) GetShardNum() int64 {
	return atomic.LoadInt64(&s.ref.ShardNum)
}

func (s *IndexShard) GetLatestShardID() int64 {
	return atomic.LoadInt64(&s.ref.ShardNum) - 1
}

func (s *IndexShard) NewShard() error {
	log.Info().
		Str("index", s.root.GetName()).
		Str("shard", s.GetID()).
		Int64("second shard", s.GetShardNum()).
		Msg("init new second layer shard")

	// update current shard
	s.root.UpdateStatsBySecondShard(s.GetID(), s.GetLatestShardID())
	s.root.lock.Lock()
	secondShard := s.shards[s.GetLatestShardID()]
	secondShard.ref.Stats.DocTimeMin = s.ref.Stats.DocTimeMin
	secondShard.ref.Stats.DocTimeMax = s.ref.Stats.DocTimeMax
	s.ref.Stats.DocTimeMin = 0
	s.ref.Stats.DocTimeMax = 0
	// create new shard
	atomic.AddInt64(&s.ref.ShardNum, 1)
	s.ref.Shards = append(s.ref.Shards, &meta.IndexSecondShard{ID: s.GetLatestShardID()})
	s.shards = append(s.shards, &IndexSecondShard{root: s.root, ref: s.ref.Shards[s.GetLatestShardID()]})
	s.root.lock.Unlock()

	// store update
	if err := storeIndex(s.root); err != nil {
		return err
	}
	return s.openWriter(s.GetLatestShardID())
}

// GetWriter return the newest shard writer or special shard writer
func (s *IndexShard) GetWriter(shardID ...int64) (*bluge.Writer, error) {
	var id int64
	if len(shardID) == 1 {
		id = shardID[0]
	} else {
		id = s.GetLatestShardID()
	}
	if id >= s.GetShardNum() || id < 0 {
		return nil, errors.New(errors.ErrorTypeRuntimeException, "second shard not found")
	}
	s.lock.RLock()
	secondShard := s.shards[id]
	s.lock.RUnlock()

	secondShard.lock.RLock()
	w := secondShard.writer
	secondShard.lock.RUnlock()
	if w != nil {
		return w, nil
	}

	// open writer
	if err := s.openWriter(id); err != nil {
		return nil, err
	}

	// check WAL
	if err := s.OpenWAL(); err != nil {
		return nil, err
	}

	secondShard.lock.RLock()
	w = secondShard.writer
	secondShard.lock.RUnlock()
	return w, nil
}

// GetWriters return all shard writers
func (s *IndexShard) GetWriters() ([]*bluge.Writer, error) {
	ws := make([]*bluge.Writer, 0, s.GetShardNum())
	for i := int64(0); i < s.GetShardNum(); i++ {
		w, err := s.GetWriter(i)
		if err != nil {
			return nil, err
		}
		ws = append(ws, w)
	}
	return ws, nil
}

// GetReaders return all shard readers
func (s *IndexShard) GetReaders(timeMin, timeMax int64) ([]*bluge.Reader, error) {
	rs := make([]*bluge.Reader, 0, 1)
	chs := make(chan *bluge.Reader, s.GetShardNum())
	eg := errgroup.Group{}
	eg.SetLimit(config.Global.Shard.GorutineNum)
	for i := s.GetLatestShardID(); i >= 0; i-- {
		i := i
		s.lock.RLock()
		secondShard := s.shards[i]
		s.lock.RUnlock()
		sMin := atomic.LoadInt64(&secondShard.ref.Stats.DocTimeMin)
		sMax := atomic.LoadInt64(&secondShard.ref.Stats.DocTimeMax)
		if (timeMin > 0 && sMax > 0 && sMax < timeMin) ||
			(timeMax > 0 && sMin > 0 && sMin > timeMax) {
			continue
		}
		eg.Go(func() error {
			w, err := s.GetWriter(i)
			if err != nil {
				return err
			}
			r, err := w.Reader()
			if err != nil {
				return err
			}
			chs <- r
			return nil
		})
		if sMin > 0 && sMin < timeMin {
			break
		}
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	close(chs)
	for r := range chs {
		rs = append(rs, r)
	}
	return rs, nil
}

func (s *IndexShard) openWriter(shardID int64) error {
	var defaultSearchAnalyzer *analysis.Analyzer
	analyzers := s.root.GetAnalyzers()
	if analyzers != nil {
		defaultSearchAnalyzer = analyzers["default"]
	}
	s.lock.RLock()
	secondShard := s.shards[shardID]
	s.lock.RUnlock()
	secondShard.lock.Lock()
	defer secondShard.lock.Unlock()
	if secondShard.writer != nil {
		return nil
	}
	var err error
	indexName := fmt.Sprintf("%s/%s/%06x", s.GetIndexName(), s.GetID(), shardID)
	secondShard.writer, err = OpenIndexWriter(indexName, s.root.GetStorageType(), defaultSearchAnalyzer, 0, 0)
	return err
}

func (s *IndexShard) Close() error {
	if atomic.LoadUint64(&s.open) == 0 {
		return nil
	}

	s.close <- struct{}{}
	atomic.StoreUint64(&s.open, 0)

	s.lock.Lock()
	defer s.lock.Unlock()
	for _, secondShard := range s.shards {
		if secondShard.writer == nil {
			continue
		}
		if err := secondShard.writer.Close(); err != nil {
			return err
		}
		secondShard.writer = nil
	}

	if err := s.wal.Close(); err != nil {
		return err
	}
	s.wal = nil

	return nil
}

func (s *IndexShard) SetTimestamp(t int64) {
	s.root.lock.Lock()
	defer s.root.lock.Unlock()
	if s.ref.Stats.DocTimeMin == 0 {
		s.ref.Stats.DocTimeMin = t
		s.ref.Stats.DocTimeMax = t
		return
	}
	if t < s.ref.Stats.DocTimeMin {
		s.ref.Stats.DocTimeMin = t
	} else if t > s.ref.Stats.DocTimeMax {
		s.ref.Stats.DocTimeMax = t
	}
}

// FindShardByDocID finds docID in which shard and returns the shard id
func (s *IndexShard) FindShardByDocID(docID string) (int64, error) {
	query := bluge.NewBooleanQuery()
	query.AddMust(bluge.NewTermQuery(docID).SetField("_id"))
	request := bluge.NewTopNSearch(1, query).WithStandardAggregations()
	ctx := context.Background()

	// check id store by which shard
	shardID := int64(-1)
	writers, err := s.GetWriters()
	if err != nil {
		return shardID, err
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(config.Global.Shard.GorutineNum)
	for id := int64(len(writers)) - 1; id >= 0; id-- {
		id := id
		w := writers[id]
		eg.Go(func() error {
			r, err := w.Reader()
			if err != nil {
				log.Error().Err(err).
					Str("index", s.GetIndexName()).
					Str("shard", s.GetID()).
					Int64("second shard", id).
					Msg("failed to get reader")
				return nil // not check err, if returns err with cancel all gorutines.
			}
			defer r.Close()
			dmi, err := r.Search(ctx, request)
			if err != nil {
				log.Error().Err(err).
					Str("index", s.GetIndexName()).
					Str("shard", s.GetID()).
					Int64("second shard", id).
					Msg("failed to do search")
				return nil // not check err, if returns err with cancel all gorutines.
			}
			if dmi.Aggregations().Count() > 0 {
				shardID = id
				return errors.ErrCancelSignal // check err, if returns err with cancel other all gorutines.
			}
			return nil
		})
	}
	_ = eg.Wait()
	if shardID == -1 {
		return shardID, errors.ErrorIDNotFound
	}
	return shardID, nil
}

// FindDocumentByDocID finds docID and returns the document
func (s *IndexShard) FindDocumentByDocID(docID string) (*meta.Hit, error) {
	query := bluge.NewBooleanQuery()
	query.AddMust(bluge.NewTermQuery(docID).SetField("_id"))
	request := bluge.NewTopNSearch(1, query).WithStandardAggregations()
	ctx := context.Background()

	// check id store by which shard
	var hit *meta.Hit
	writers, err := s.GetWriters()
	if err != nil {
		return nil, err
	}

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(config.Global.Shard.GorutineNum)
	for id := int64(len(writers)) - 1; id >= 0; id-- {
		id := id
		w := writers[id]
		eg.Go(func() error {
			r, err := w.Reader()
			if err != nil {
				log.Error().Err(err).
					Str("index", s.GetIndexName()).
					Str("shard", s.GetID()).
					Int64("second shard", id).
					Msg("failed to get reader")
				return nil // not check err, if returns err with cancel all gorutines.
			}
			defer r.Close()
			dmi, err := r.Search(ctx, request)
			if err != nil {
				log.Error().Err(err).
					Str("index", s.GetIndexName()).
					Str("shard", s.GetID()).
					Int64("second shard", id).
					Msg("failed to do search")
				return nil // not check err, if returns err with cancel all gorutines.
			}
			if dmi.Aggregations().Count() > 0 {
				var id string
				var indexName string
				var timestamp time.Time
				var sourceData map[string]interface{}
				if next, err := dmi.Next(); err == nil {
					_ = next.VisitStoredFields(func(field string, value []byte) bool {
						switch field {
						case "_id":
							id = string(value)
						case "_index":
							indexName = string(value)
						case "@timestamp":
							timestamp, _ = bluge.DecodeDateTime(value)
						case "_source":
							sourceData = source.Response(&meta.Source{Enable: true}, value)
						default: // do nothing
						}
						return true
					})
				}
				hit = &meta.Hit{
					Index:     indexName,
					Type:      "_doc",
					ID:        id,
					Score:     0,
					Timestamp: timestamp,
					Source:    sourceData,
				}
				return errors.ErrCancelSignal // check err, if returns err with cancel other all gorutines.
			}

			return nil
		})
	}
	_ = eg.Wait()
	if hit == nil {
		return nil, errors.ErrorIDNotFound
	}
	return hit, nil
}
