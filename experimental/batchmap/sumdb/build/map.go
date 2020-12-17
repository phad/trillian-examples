// Copyright 2020 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// map constructs a verifiable map from the modules in Go SumDB.
package main

import (
	"context"
	"crypto"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"reflect"

	"github.com/apache/beam/sdks/go/pkg/beam"
	"github.com/apache/beam/sdks/go/pkg/beam/io/databaseio"
	beamlog "github.com/apache/beam/sdks/go/pkg/beam/log"
	"github.com/apache/beam/sdks/go/pkg/beam/x/beamx"

	"github.com/golang/glog"

	"github.com/google/trillian/experimental/batchmap"

	"github.com/google/trillian-examples/experimental/batchmap/sumdb/build/pipeline"
	"github.com/google/trillian-examples/experimental/batchmap/sumdb/mapdb"

	_ "github.com/mattn/go-sqlite3"
)

const hash = crypto.SHA512_256

var (
	sumDBString       = flag.String("sum_db", "", "The path of the SQLite file generated by sumdbaudit, e.g. ~/sum.db.")
	mapDBString       = flag.String("map_db", "", "Output database where the map tiles will be written.")
	treeID            = flag.Int64("tree_id", 12345, "The ID of the tree. Used as a salt in hashing.")
	prefixStrata      = flag.Int("prefix_strata", 2, "The number of strata of 8-bit strata before the final strata.")
	count             = flag.Int64("count", -1, "The total number of entries starting from the beginning of the SumDB to use, or -1 to use all")
	batchSize         = flag.Int("write_batch_size", 250, "Number of tiles to write per batch")
	incrementalUpdate = flag.Bool("incremental_update", false, "If set the map tiles from the previous revision will be updated with the delta, otherwise this will build the map from scratch each time.")
	buildVersionList  = flag.Bool("build_version_list", false, "If set then the map will also contain a mapping for each module to a log committing to its list of versions.")
)

func init() {
	beam.RegisterType(reflect.TypeOf((*tileToDBRowFn)(nil)).Elem())
	beam.RegisterFunction(tileFromDBRowFn)
}

func main() {
	flag.Parse()

	if *buildVersionList && *incrementalUpdate {
		glog.Exitf("Unsupported: build_version_list cannot be used with incremental_update")
	}
	// Connect to where we will read from and write to.
	sumDB, err := newSumDBMirrorFromFlags()
	if err != nil {
		glog.Exitf("Failed to initialize from local SumDB: %v", err)
	}
	mapDB, rev, err := sinkFromFlags()
	if err != nil {
		glog.Exitf("Failed to initialize Map DB: %v", err)
	}

	// Pull out latest information from each DB.
	golden, totalLeaves, err := sumDB.getEntryMetadata()
	if err != nil {
		glog.Exitf("Failed to get latest SumDB entry metadata: %v", err)
	}
	if totalLeaves < *count {
		glog.Exitf("Wanted %d leaves but only %d available", *count, totalLeaves)
	}

	// endID is determined by the count flag and the number of entries available in SumDB.
	var endID int64
	if *count < 0 {
		endID = totalLeaves
	} else {
		endID = *count
	}

	// startID is 0, unless we are updating the map and there is a previous revision.
	var startID int64
	var lastMapRev int
	if *incrementalUpdate {
		lastMapRev, _, startID, err = mapDB.LatestRevision()
		if err != nil {
			glog.Exitf("Failed to get LatestRevision: %v", err)
		}
		if startID >= endID {
			glog.Exitf("startID >= endID (%d > %d)", startID, endID)
		}
	}

	beam.Init()
	beamlog.SetLogger(&BeamGLogger{InfoLogAtVerbosity: 2})
	p, s := beam.NewPipelineWithRoot()
	records := sumDB.beamSource(s.Scope("source"), startID, endID)
	entries := pipeline.CreateEntries(s, *treeID, records)

	if *buildVersionList {
		entries = beam.Flatten(s, entries, pipeline.MakeVersionList(s, records))
	}

	var allTiles beam.PCollection
	if *incrementalUpdate {
		glog.Infof("Updating revision %d with range [%d, %d)", lastMapRev, startID, endID)
		mapTiles := databaseio.Query(s, "sqlite3", *mapDBString, fmt.Sprintf("SELECT * FROM tiles WHERE revision=%d", lastMapRev), reflect.TypeOf(MapTile{}))
		allTiles, err = batchmap.Update(s, beam.ParDo(s, tileFromDBRowFn, mapTiles), entries, *treeID, hash, *prefixStrata)
	} else {
		glog.Infof("Creating new map revision from range [0, %d)", endID)
		allTiles, err = batchmap.Create(s, entries, *treeID, hash, *prefixStrata)
	}
	if err != nil {
		glog.Exitf("Failed to create pipeline: %q", err)
	}

	rows := beam.ParDo(s.Scope("convertoutput"), &tileToDBRowFn{Revision: rev}, allTiles)
	databaseio.WriteWithBatchSize(s.Scope("sink"), *batchSize, "sqlite3", *mapDBString, "tiles", []string{}, rows)

	// All of the above constructs the pipeline but doesn't run it. Now we run it.
	if err := beamx.Run(context.Background(), p); err != nil {
		glog.Exitf("Failed to execute job: %q", err)
	}

	if err := mapDB.WriteRevision(rev, golden, endID); err != nil {
		glog.Exitf("Failed to finalize map revison %d: %v", rev, err)
	}
}

func sinkFromFlags() (*mapdb.TileDB, int, error) {
	if len(*mapDBString) == 0 {
		return nil, 0, fmt.Errorf("missing flag: map_db")
	}

	tiledb, err := mapdb.NewTileDB(*mapDBString)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open map DB at %q: %v", *mapDBString, err)
	}
	if err := tiledb.Init(); err != nil {
		return nil, 0, fmt.Errorf("failed to Init map DB at %q: %v", *mapDBString, err)
	}

	var rev int
	if rev, err = tiledb.NextWriteRevision(); err != nil {
		return nil, 0, fmt.Errorf("failed to query for next write revision: %v", err)

	}
	return tiledb, rev, nil
}

// MapTile is the schema format of the Map database to allow for databaseio writing.
type MapTile struct {
	Revision int
	Path     []byte
	Tile     []byte
}

type tileToDBRowFn struct {
	Revision int
}

func (fn *tileToDBRowFn) ProcessElement(ctx context.Context, t *batchmap.Tile) (MapTile, error) {
	bs, err := json.Marshal(t)
	if err != nil {
		return MapTile{}, err
	}
	return MapTile{
		Revision: fn.Revision,
		Path:     t.Path,
		Tile:     bs,
	}, nil
}

func tileFromDBRowFn(t MapTile) (*batchmap.Tile, error) {
	var res batchmap.Tile
	if err := json.Unmarshal(t.Tile, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type sumDBMirror struct {
	dbString string
	db       *sql.DB
}

func newSumDBMirrorFromFlags() (*sumDBMirror, error) {
	if len(*sumDBString) == 0 {
		return nil, fmt.Errorf("missing flag: sum_db")
	}
	db, err := sql.Open("sqlite3", *sumDBString)
	return &sumDBMirror{
		dbString: *sumDBString,
		db:       db,
	}, err
}

// getEntryMetadata gets the STH and the total number of entries available to process.
func (m *sumDBMirror) getEntryMetadata() ([]byte, int64, error) {
	var cp []byte
	var leafCount int64

	if err := m.db.QueryRow("SELECT checkpoint FROM checkpoints ORDER BY datetime DESC LIMIT 1").Scan(&cp); err != nil {
		return nil, 0, err
	}
	return cp, leafCount, m.db.QueryRow("SELECT COUNT(*) FROM leafMetadata").Scan(&leafCount)
}

// beamSource returns a PCollection of Metadata, containing entries in range [start, end).
func (m *sumDBMirror) beamSource(s beam.Scope, start, end int64) beam.PCollection {
	return databaseio.Query(s, "sqlite3", m.dbString, fmt.Sprintf("SELECT * FROM leafMetadata WHERE id >= %d AND id < %d", start, end), reflect.TypeOf(pipeline.Metadata{}))
}

// BeamGLogger allows Beam to log via the glog mechanism.
// This is used to allow the very verbose logging output from Beam to be switched off.
type BeamGLogger struct {
	InfoLogAtVerbosity glog.Level
}

// Log logs.
func (l *BeamGLogger) Log(ctx context.Context, sev beamlog.Severity, _ int, msg string) {
	switch sev {
	case beamlog.SevDebug:
		glog.V(3).Info(msg)
	case beamlog.SevInfo:
		glog.V(l.InfoLogAtVerbosity).Info(msg)
	case beamlog.SevError:
		glog.Error(msg)
	case beamlog.SevWarn:
		glog.Warning(msg)
	default:
		glog.V(5).Infof("?? %s", msg)
	}
}
