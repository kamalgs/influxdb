package tsm1_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/influxdata/influxdb/logger"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/pkg/deep"
	"github.com/influxdata/influxdb/query"
	"github.com/influxdata/influxdb/tsdb"
	"github.com/influxdata/influxdb/tsdb/engine/tsm1"
	"github.com/influxdata/influxdb/tsdb/index/inmem"
	"github.com/influxdata/influxql"
)

// Ensure that deletes only sent to the WAL will clear out the data from the cache on restart
func TestEngine_DeleteWALLoadMetadata(t *testing.T) {
	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			e := MustOpenEngine(index)
			defer e.Close()

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=B value=1.2 2000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			// Remove series.
			itr := &seriesIterator{keys: [][]byte{[]byte("cpu,host=A")}}
			if err := e.DeleteSeriesRange(itr, math.MinInt64, math.MaxInt64, false); err != nil {
				t.Fatalf("failed to delete series: %s", err.Error())
			}

			// Ensure we can close and load index from the WAL
			if err := e.Reopen(); err != nil {
				t.Fatal(err)
			}

			if exp, got := 0, len(e.Cache.Values(tsm1.SeriesFieldKeyBytes("cpu,host=A", "value"))); exp != got {
				t.Fatalf("unexpected number of values: got: %d. exp: %d", got, exp)
			}

			if exp, got := 1, len(e.Cache.Values(tsm1.SeriesFieldKeyBytes("cpu,host=B", "value"))); exp != got {
				t.Fatalf("unexpected number of values: got: %d. exp: %d", got, exp)
			}
		})
	}
}

// Ensure that the engine can write & read shard digest files.
func TestEngine_Digest(t *testing.T) {
	e := MustOpenEngine(inmem.IndexName)
	defer e.Close()

	if err := e.Open(); err != nil {
		t.Fatalf("failed to open tsm1 engine: %s", err.Error())
	}

	// Create a few points.
	points := []models.Point{
		MustParsePointString("cpu,host=A value=1.1 1000000000"),
		MustParsePointString("cpu,host=B value=1.2 2000000000"),
	}

	if err := e.WritePoints(points); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}

	// Force a compaction.
	e.ScheduleFullCompaction()

	digest := func() ([]span, error) {
		// Get a reader for the shard's digest.
		r, sz, err := e.Digest()
		if err != nil {
			return nil, err
		}

		if sz <= 0 {
			t.Fatalf("expected digest size > 0")
		}

		// Make sure the digest can be read.
		dr, err := tsm1.NewDigestReader(r)
		if err != nil {
			r.Close()
			return nil, err
		}
		defer dr.Close()

		got := []span{}

		for {
			k, s, err := dr.ReadTimeSpan()
			if err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}

			got = append(got, span{
				key:   k,
				tspan: s,
			})
		}

		return got, nil
	}

	exp := []span{
		span{
			key: "cpu,host=A#!~#value",
			tspan: &tsm1.DigestTimeSpan{
				Ranges: []tsm1.DigestTimeRange{
					tsm1.DigestTimeRange{
						Min: 1000000000,
						Max: 1000000000,
						N:   1,
						CRC: 1048747083,
					},
				},
			},
		},
		span{
			key: "cpu,host=B#!~#value",
			tspan: &tsm1.DigestTimeSpan{
				Ranges: []tsm1.DigestTimeRange{
					tsm1.DigestTimeRange{
						Min: 2000000000,
						Max: 2000000000,
						N:   1,
						CRC: 734984746,
					},
				},
			},
		},
	}

	for n := 0; n < 2; n++ {
		got, err := digest()
		if err != nil {
			t.Fatalf("n = %d: %s", n, err)
		}

		// Make sure the data in the digest was valid.
		if !reflect.DeepEqual(exp, got) {
			t.Fatalf("n = %d\nexp = %v\ngot = %v\n", n, exp, got)
		}
	}

	// Test that writing more points causes the digest to be updated.
	points = []models.Point{
		MustParsePointString("cpu,host=C value=1.1 3000000000"),
	}

	if err := e.WritePoints(points); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}

	// Force a compaction.
	e.ScheduleFullCompaction()

	// Get new digest.
	got, err := digest()
	if err != nil {
		t.Fatal(err)
	}

	exp = append(exp, span{
		key: "cpu,host=C#!~#value",
		tspan: &tsm1.DigestTimeSpan{
			Ranges: []tsm1.DigestTimeRange{
				tsm1.DigestTimeRange{
					Min: 3000000000,
					Max: 3000000000,
					N:   1,
					CRC: 2553233514,
				},
			},
		},
	})

	if !reflect.DeepEqual(exp, got) {
		t.Fatalf("\nexp = %v\ngot = %v\n", exp, got)
	}
}

type span struct {
	key   string
	tspan *tsm1.DigestTimeSpan
}

// Ensure that the engine will backup any TSM files created since the passed in time
func TestEngine_Backup(t *testing.T) {
	sfile := MustOpenSeriesFile()
	defer sfile.Close()

	// Generate temporary file.
	f, _ := ioutil.TempFile("", "tsm")
	f.Close()
	os.Remove(f.Name())
	walPath := filepath.Join(f.Name(), "wal")
	os.MkdirAll(walPath, 0777)
	defer os.RemoveAll(f.Name())

	// Create a few points.
	p1 := MustParsePointString("cpu,host=A value=1.1 1000000000")
	p2 := MustParsePointString("cpu,host=B value=1.2 2000000000")
	p3 := MustParsePointString("cpu,host=C value=1.3 3000000000")

	// Write those points to the engine.
	db := path.Base(f.Name())
	opt := tsdb.NewEngineOptions()
	opt.InmemIndex = inmem.NewIndex(db, sfile.SeriesFile)
	idx := tsdb.MustOpenIndex(1, db, filepath.Join(f.Name(), "index"), sfile.SeriesFile, opt)
	defer idx.Close()

	e := tsm1.NewEngine(1, idx, db, f.Name(), walPath, sfile.SeriesFile, opt).(*tsm1.Engine)

	// mock the planner so compactions don't run during the test
	e.CompactionPlan = &mockPlanner{}

	if err := e.Open(); err != nil {
		t.Fatalf("failed to open tsm1 engine: %s", err.Error())
	}

	if err := e.WritePoints([]models.Point{p1}); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}
	if err := e.WriteSnapshot(); err != nil {
		t.Fatalf("failed to snapshot: %s", err.Error())
	}

	if err := e.WritePoints([]models.Point{p2}); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}

	b := bytes.NewBuffer(nil)
	if err := e.Backup(b, "", time.Unix(0, 0)); err != nil {
		t.Fatalf("failed to backup: %s", err.Error())
	}

	tr := tar.NewReader(b)
	if len(e.FileStore.Files()) != 2 {
		t.Fatalf("file count wrong: exp: %d, got: %d", 2, len(e.FileStore.Files()))
	}

	fileNames := map[string]bool{}
	for _, f := range e.FileStore.Files() {
		fileNames[filepath.Base(f.Path())] = true
	}

	th, err := tr.Next()
	for err == nil {
		if !fileNames[th.Name] {
			t.Errorf("Extra file in backup: %q", th.Name)
		}
		delete(fileNames, th.Name)
		th, err = tr.Next()
	}

	if err != nil && err != io.EOF {
		t.Fatalf("Problem reading tar header: %s", err)
	}

	for f := range fileNames {
		t.Errorf("File missing from backup: %s", f)
	}

	if t.Failed() {
		t.FailNow()
	}

	lastBackup := time.Now()

	// we have to sleep for a second because last modified times only have second level precision.
	// so this test won't work properly unless the file is at least a second past the last one
	time.Sleep(time.Second)

	if err := e.WritePoints([]models.Point{p3}); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}

	b = bytes.NewBuffer(nil)
	if err := e.Backup(b, "", lastBackup); err != nil {
		t.Fatalf("failed to backup: %s", err.Error())
	}

	tr = tar.NewReader(b)
	th, err = tr.Next()
	if err != nil {
		t.Fatalf("error getting next tar header: %s", err.Error())
	}

	mostRecentFile := e.FileStore.Files()[e.FileStore.Count()-1].Path()
	if !strings.Contains(mostRecentFile, th.Name) || th.Name == "" {
		t.Fatalf("file name doesn't match:\n\tgot: %s\n\texp: %s", th.Name, mostRecentFile)
	}
}

func TestEngine_Export(t *testing.T) {
	// Generate temporary file.
	f, _ := ioutil.TempFile("", "tsm")
	f.Close()
	os.Remove(f.Name())
	walPath := filepath.Join(f.Name(), "wal")
	os.MkdirAll(walPath, 0777)
	defer os.RemoveAll(f.Name())

	// Create a few points.
	p1 := MustParsePointString("cpu,host=A value=1.1 1000000000")
	p2 := MustParsePointString("cpu,host=B value=1.2 2000000000")
	p3 := MustParsePointString("cpu,host=C value=1.3 3000000000")

	sfile := MustOpenSeriesFile()
	defer sfile.Close()

	// Write those points to the engine.
	db := path.Base(f.Name())
	opt := tsdb.NewEngineOptions()
	opt.InmemIndex = inmem.NewIndex(db, sfile.SeriesFile)
	idx := tsdb.MustOpenIndex(1, db, filepath.Join(f.Name(), "index"), sfile.SeriesFile, opt)
	defer idx.Close()

	e := tsm1.NewEngine(1, idx, db, f.Name(), walPath, sfile.SeriesFile, opt).(*tsm1.Engine)

	// mock the planner so compactions don't run during the test
	e.CompactionPlan = &mockPlanner{}

	if err := e.Open(); err != nil {
		t.Fatalf("failed to open tsm1 engine: %s", err.Error())
	}

	if err := e.WritePoints([]models.Point{p1}); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}
	if err := e.WriteSnapshot(); err != nil {
		t.Fatalf("failed to snapshot: %s", err.Error())
	}

	if err := e.WritePoints([]models.Point{p2}); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}
	if err := e.WriteSnapshot(); err != nil {
		t.Fatalf("failed to snapshot: %s", err.Error())
	}

	if err := e.WritePoints([]models.Point{p3}); err != nil {
		t.Fatalf("failed to write points: %s", err.Error())
	}

	// export the whole DB
	var exBuf bytes.Buffer
	if err := e.Export(&exBuf, "", time.Unix(0, 0), time.Unix(0, 4000000000)); err != nil {
		t.Fatalf("failed to export: %s", err.Error())
	}

	var bkBuf bytes.Buffer
	if err := e.Backup(&bkBuf, "", time.Unix(0, 0)); err != nil {
		t.Fatalf("failed to backup: %s", err.Error())
	}

	if len(e.FileStore.Files()) != 3 {
		t.Fatalf("file count wrong: exp: %d, got: %d", 3, len(e.FileStore.Files()))
	}

	fileNames := map[string]bool{}
	for _, f := range e.FileStore.Files() {
		fileNames[filepath.Base(f.Path())] = true
	}

	fileData, err := getExportData(&exBuf)
	if err != nil {
		t.Errorf("Error extracting data from export: %s", err.Error())
	}

	// TEST 1: did we get any extra files not found in the store?
	for k, _ := range fileData {
		if _, ok := fileNames[k]; !ok {
			t.Errorf("exported a file not in the store: %s", k)
		}
	}

	// TEST 2: did we miss any files that the store had?
	for k, _ := range fileNames {
		if _, ok := fileData[k]; !ok {
			t.Errorf("failed to export a file from the store: %s", k)
		}
	}

	// TEST 3: Does 'backup' get the same files + bits?
	tr := tar.NewReader(&bkBuf)

	th, err := tr.Next()
	for err == nil {
		expData, ok := fileData[th.Name]
		if !ok {
			t.Errorf("Extra file in backup: %q", th.Name)
			continue
		}

		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, tr); err != nil {
			t.Fatal(err)
		}

		if !equalBuffers(expData, buf) {
			t.Errorf("2Difference in data between backup and Export for file %s", th.Name)
		}

		th, err = tr.Next()
	}

	if t.Failed() {
		t.FailNow()
	}

	// TEST 4:  Are subsets (1), (2), (3), (1,2), (2,3) accurately found in the larger export?
	// export the whole DB
	var ex1 bytes.Buffer
	if err := e.Export(&ex1, "", time.Unix(0, 0), time.Unix(0, 1000000000)); err != nil {
		t.Fatalf("failed to export: %s", err.Error())
	}
	ex1Data, err := getExportData(&ex1)
	if err != nil {
		t.Errorf("Error extracting data from export: %s", err.Error())
	}

	for k, v := range ex1Data {
		fullExp, ok := fileData[k]
		if !ok {
			t.Errorf("Extracting subset resulted in file not found in full export: %s", err.Error())
			continue
		}
		if !equalBuffers(fullExp, v) {
			t.Errorf("2Difference in data between backup and Export for file %s", th.Name)
		}

	}

	var ex2 bytes.Buffer
	if err := e.Export(&ex2, "", time.Unix(0, 1000000001), time.Unix(0, 2000000000)); err != nil {
		t.Fatalf("failed to export: %s", err.Error())
	}

	ex2Data, err := getExportData(&ex2)
	if err != nil {
		t.Errorf("Error extracting data from export: %s", err.Error())
	}

	for k, v := range ex2Data {
		fullExp, ok := fileData[k]
		if !ok {
			t.Errorf("Extracting subset resulted in file not found in full export: %s", err.Error())
			continue
		}
		if !equalBuffers(fullExp, v) {
			t.Errorf("2Difference in data between backup and Export for file %s", th.Name)
		}

	}

	var ex3 bytes.Buffer
	if err := e.Export(&ex3, "", time.Unix(0, 2000000001), time.Unix(0, 3000000000)); err != nil {
		t.Fatalf("failed to export: %s", err.Error())
	}

	ex3Data, err := getExportData(&ex3)
	if err != nil {
		t.Errorf("Error extracting data from export: %s", err.Error())
	}

	for k, v := range ex3Data {
		fullExp, ok := fileData[k]
		if !ok {
			t.Errorf("Extracting subset resulted in file not found in full export: %s", err.Error())
			continue
		}
		if !equalBuffers(fullExp, v) {
			t.Errorf("2Difference in data between backup and Export for file %s", th.Name)
		}

	}

	var ex12 bytes.Buffer
	if err := e.Export(&ex12, "", time.Unix(0, 0), time.Unix(0, 2000000000)); err != nil {
		t.Fatalf("failed to export: %s", err.Error())
	}

	ex12Data, err := getExportData(&ex12)
	if err != nil {
		t.Errorf("Error extracting data from export: %s", err.Error())
	}

	for k, v := range ex12Data {
		fullExp, ok := fileData[k]
		if !ok {
			t.Errorf("Extracting subset resulted in file not found in full export: %s", err.Error())
			continue
		}
		if !equalBuffers(fullExp, v) {
			t.Errorf("2Difference in data between backup and Export for file %s", th.Name)
		}

	}

	var ex23 bytes.Buffer
	if err := e.Export(&ex23, "", time.Unix(0, 1000000001), time.Unix(0, 3000000000)); err != nil {
		t.Fatalf("failed to export: %s", err.Error())
	}

	ex23Data, err := getExportData(&ex23)
	if err != nil {
		t.Errorf("Error extracting data from export: %s", err.Error())
	}

	for k, v := range ex23Data {
		fullExp, ok := fileData[k]
		if !ok {
			t.Errorf("Extracting subset resulted in file not found in full export: %s", err.Error())
			continue
		}
		if !equalBuffers(fullExp, v) {
			t.Errorf("2Difference in data between backup and Export for file %s", th.Name)
		}

	}
}

func equalBuffers(bufA, bufB *bytes.Buffer) bool {
	for i, v := range bufA.Bytes() {
		if v != bufB.Bytes()[i] {
			return false
		}
	}
	return true
}

func getExportData(exBuf *bytes.Buffer) (map[string]*bytes.Buffer, error) {

	tr := tar.NewReader(exBuf)

	fileData := make(map[string]*bytes.Buffer)

	// TEST 1: Get the bits for each file.  If we got a file the store doesn't know about, report error
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, tr); err != nil {
			return nil, err
		}
		fileData[th.Name] = buf

	}

	return fileData, nil
}

// Ensure engine can create an ascending iterator for cached values.
func TestEngine_CreateIterator_Cache_Ascending(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=A value=1.2 2000000000`,
				`cpu,host=A value=1.3 3000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			itr, err := e.CreateIterator(context.Background(), "cpu", query.IteratorOptions{
				Expr:       influxql.MustParseExpr(`value`),
				Dimensions: []string{"host"},
				StartTime:  influxql.MinTime,
				EndTime:    influxql.MaxTime,
				Ascending:  true,
			})
			if err != nil {
				t.Fatal(err)
			}
			fitr := itr.(query.FloatIterator)

			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(0): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 1000000000, Value: 1.1}) {
				t.Fatalf("unexpected point(0): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(1): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 2000000000, Value: 1.2}) {
				t.Fatalf("unexpected point(1): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(2): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 3000000000, Value: 1.3}) {
				t.Fatalf("unexpected point(2): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("expected eof, got error: %v", err)
			} else if p != nil {
				t.Fatalf("expected eof: %v", p)
			}
		})
	}
}

// Ensure engine can create an descending iterator for cached values.
func TestEngine_CreateIterator_Cache_Descending(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {

			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=A value=1.2 2000000000`,
				`cpu,host=A value=1.3 3000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			itr, err := e.CreateIterator(context.Background(), "cpu", query.IteratorOptions{
				Expr:       influxql.MustParseExpr(`value`),
				Dimensions: []string{"host"},
				StartTime:  influxql.MinTime,
				EndTime:    influxql.MaxTime,
				Ascending:  false,
			})
			if err != nil {
				t.Fatal(err)
			}
			fitr := itr.(query.FloatIterator)

			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(0): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 3000000000, Value: 1.3}) {
				t.Fatalf("unexpected point(0): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unepxected error(1): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 2000000000, Value: 1.2}) {
				t.Fatalf("unexpected point(1): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(2): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 1000000000, Value: 1.1}) {
				t.Fatalf("unexpected point(2): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("expected eof, got error: %v", err)
			} else if p != nil {
				t.Fatalf("expected eof: %v", p)
			}
		})
	}
}

// Ensure engine can create an ascending iterator for tsm values.
func TestEngine_CreateIterator_TSM_Ascending(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=A value=1.2 2000000000`,
				`cpu,host=A value=1.3 3000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			e.MustWriteSnapshot()

			itr, err := e.CreateIterator(context.Background(), "cpu", query.IteratorOptions{
				Expr:       influxql.MustParseExpr(`value`),
				Dimensions: []string{"host"},
				StartTime:  1000000000,
				EndTime:    3000000000,
				Ascending:  true,
			})
			if err != nil {
				t.Fatal(err)
			}
			fitr := itr.(query.FloatIterator)

			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(0): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 1000000000, Value: 1.1}) {
				t.Fatalf("unexpected point(0): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(1): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 2000000000, Value: 1.2}) {
				t.Fatalf("unexpected point(1): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(2): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 3000000000, Value: 1.3}) {
				t.Fatalf("unexpected point(2): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("expected eof, got error: %v", err)
			} else if p != nil {
				t.Fatalf("expected eof: %v", p)
			}
		})
	}
}

// Ensure engine can create an descending iterator for cached values.
func TestEngine_CreateIterator_TSM_Descending(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=A value=1.2 2000000000`,
				`cpu,host=A value=1.3 3000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			e.MustWriteSnapshot()

			itr, err := e.CreateIterator(context.Background(), "cpu", query.IteratorOptions{
				Expr:       influxql.MustParseExpr(`value`),
				Dimensions: []string{"host"},
				StartTime:  influxql.MinTime,
				EndTime:    influxql.MaxTime,
				Ascending:  false,
			})
			if err != nil {
				t.Fatal(err)
			}
			fitr := itr.(query.FloatIterator)

			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(0): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 3000000000, Value: 1.3}) {
				t.Fatalf("unexpected point(0): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(1): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 2000000000, Value: 1.2}) {
				t.Fatalf("unexpected point(1): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(2): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 1000000000, Value: 1.1}) {
				t.Fatalf("unexpected point(2): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("expected eof, got error: %v", err)
			} else if p != nil {
				t.Fatalf("expected eof: %v", p)
			}
		})
	}
}

// Ensure engine can create an iterator with auxilary fields.
func TestEngine_CreateIterator_Aux(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("F"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=A F=100 1000000000`,
				`cpu,host=A value=1.2 2000000000`,
				`cpu,host=A value=1.3 3000000000`,
				`cpu,host=A F=200 3000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			itr, err := e.CreateIterator(context.Background(), "cpu", query.IteratorOptions{
				Expr:       influxql.MustParseExpr(`value`),
				Aux:        []influxql.VarRef{{Val: "F"}},
				Dimensions: []string{"host"},
				StartTime:  influxql.MinTime,
				EndTime:    influxql.MaxTime,
				Ascending:  true,
			})
			if err != nil {
				t.Fatal(err)
			}
			fitr := itr.(query.FloatIterator)

			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(0): %v", err)
			} else if !deep.Equal(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 1000000000, Value: 1.1, Aux: []interface{}{float64(100)}}) {
				t.Fatalf("unexpected point(0): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(1): %v", err)
			} else if !deep.Equal(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 2000000000, Value: 1.2, Aux: []interface{}{(*float64)(nil)}}) {
				t.Fatalf("unexpected point(1): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(2): %v", err)
			} else if !deep.Equal(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 3000000000, Value: 1.3, Aux: []interface{}{float64(200)}}) {
				t.Fatalf("unexpected point(2): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("expected eof, got error: %v", err)
			} else if p != nil {
				t.Fatalf("expected eof: %v", p)
			}
		})
	}
}

// Ensure engine can create an iterator with a condition.
func TestEngine_CreateIterator_Condition(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("X"), influxql.Float)
			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("Y"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))
			e.SetFieldName([]byte("cpu"), "X")
			e.SetFieldName([]byte("cpu"), "Y")

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1000000000`,
				`cpu,host=A X=10 1000000000`,
				`cpu,host=A Y=100 1000000000`,

				`cpu,host=A value=1.2 2000000000`,

				`cpu,host=A value=1.3 3000000000`,
				`cpu,host=A X=20 3000000000`,
				`cpu,host=A Y=200 3000000000`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			itr, err := e.CreateIterator(context.Background(), "cpu", query.IteratorOptions{
				Expr:       influxql.MustParseExpr(`value`),
				Dimensions: []string{"host"},
				Condition:  influxql.MustParseExpr(`X = 10 OR Y > 150`),
				StartTime:  influxql.MinTime,
				EndTime:    influxql.MaxTime,
				Ascending:  true,
			})
			if err != nil {
				t.Fatal(err)
			}
			fitr := itr.(query.FloatIterator)

			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected error(0): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 1000000000, Value: 1.1}) {
				t.Fatalf("unexpected point(0): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("unexpected point(1): %v", err)
			} else if !reflect.DeepEqual(p, &query.FloatPoint{Name: "cpu", Tags: ParseTags("host=A"), Time: 3000000000, Value: 1.3}) {
				t.Fatalf("unexpected point(1): %v", p)
			}
			if p, err := fitr.Next(); err != nil {
				t.Fatalf("expected eof, got error: %v", err)
			} else if p != nil {
				t.Fatalf("expected eof: %v", p)
			}
		})
	}
}

// Ensures that deleting series from TSM files with multiple fields removes all the
// series from the TSM files but leaves the series in the index intact.
func TestEngine_DeleteSeries(t *testing.T) {
	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			// Create a few points.
			p1 := MustParsePointString("cpu,host=A value=1.1 1000000000")
			p2 := MustParsePointString("cpu,host=B value=1.2 2000000000")
			p3 := MustParsePointString("cpu,host=A sum=1.3 3000000000")

			e, err := NewEngine(index)
			if err != nil {
				t.Fatal(err)
			}

			// mock the planner so compactions don't run during the test
			e.CompactionPlan = &mockPlanner{}
			if err := e.Open(); err != nil {
				t.Fatal(err)
			}
			defer e.Close()

			for _, p := range []models.Point{p1, p2, p3} {
				if err := e.CreateSeriesIfNotExists(p.Key(), p.Name(), p.Tags()); err != nil {
					t.Fatalf("create series index error: %v", err)
				}
			}

			if err := e.WritePoints([]models.Point{p1, p2, p3}); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			if err := e.WriteSnapshot(); err != nil {
				t.Fatalf("failed to snapshot: %s", err.Error())
			}

			keys := e.FileStore.Keys()
			if exp, got := 3, len(keys); exp != got {
				t.Fatalf("series count mismatch: exp %v, got %v", exp, got)
			}

			itr := &seriesIterator{keys: [][]byte{[]byte("cpu,host=A")}}
			if err := e.DeleteSeriesRange(itr, math.MinInt64, math.MaxInt64, false); err != nil {
				t.Fatalf("failed to delete series: %v", err)
			}

			keys = e.FileStore.Keys()
			if exp, got := 1, len(keys); exp != got {
				t.Fatalf("series count mismatch: exp %v, got %v", exp, got)
			}

			exp := "cpu,host=B#!~#value"
			if _, ok := keys[exp]; !ok {
				t.Fatalf("wrong series deleted: exp %v, got %v", exp, keys)
			}

			// Deleting all the TSM values for a single series should still leave
			// the series in the index intact.
			indexSet := tsdb.IndexSet{Indexes: []tsdb.Index{e.index}, SeriesFile: e.sfile}
			iter, err := indexSet.MeasurementSeriesIDIterator([]byte("cpu"))
			if err != nil {
				t.Fatalf("iterator error: %v", err)
			} else if iter == nil {
				t.Fatal("nil iterator")
			}
			defer iter.Close()

			var gotKeys []string
			expKeys := []string{"cpu,host=A", "cpu,host=B"}

			for {
				elem, err := iter.Next()
				if err != nil {
					t.Fatal(err)
				}
				if elem.SeriesID == 0 {
					break
				}

				// Lookup series.
				name, tags := e.sfile.Series(elem.SeriesID)
				gotKeys = append(gotKeys, string(models.MakeKey(name, tags)))
			}

			if !reflect.DeepEqual(gotKeys, expKeys) {
				t.Fatalf("got keys %v, expected %v", gotKeys, expKeys)
			}
		})
	}
}

// Ensures that deleting series from TSM files over a range of time deleted the
// series from the TSM files but leaves the series in the index.
func TestEngine_DeleteSeriesRange(t *testing.T) {
	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			// Create a few points.
			p1 := MustParsePointString("cpu,host=0 value=1.1 6000000000")
			p2 := MustParsePointString("cpu,host=A value=1.2 2000000000")
			p3 := MustParsePointString("cpu,host=A value=1.3 3000000000")
			p4 := MustParsePointString("cpu,host=B value=1.3 4000000000")
			p5 := MustParsePointString("cpu,host=B value=1.3 5000000000")
			p6 := MustParsePointString("cpu,host=C value=1.3 1000000000")
			p7 := MustParsePointString("mem,host=C value=1.3 1000000000")
			p8 := MustParsePointString("disk,host=C value=1.3 1000000000")

			e, err := NewEngine(index)
			if err != nil {
				t.Fatal(err)
			}

			// mock the planner so compactions don't run during the test
			e.CompactionPlan = &mockPlanner{}
			if err := e.Open(); err != nil {
				t.Fatal(err)
			}
			defer e.Close()

			for _, p := range []models.Point{p1, p2, p3, p4, p5, p6, p7, p8} {
				if err := e.CreateSeriesIfNotExists(p.Key(), p.Name(), p.Tags()); err != nil {
					t.Fatalf("create series index error: %v", err)
				}
			}

			if err := e.WritePoints([]models.Point{p1, p2, p3, p4, p5, p6, p7, p8}); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			if err := e.WriteSnapshot(); err != nil {
				t.Fatalf("failed to snapshot: %s", err.Error())
			}

			keys := e.FileStore.Keys()
			if exp, got := 6, len(keys); exp != got {
				t.Fatalf("series count mismatch: exp %v, got %v", exp, got)
			}

			itr := &seriesIterator{keys: [][]byte{[]byte("cpu,host=0"), []byte("cpu,host=A"), []byte("cpu,host=B"), []byte("cpu,host=C")}}
			if err := e.DeleteSeriesRange(itr, 0, 3000000000, false); err != nil {
				t.Fatalf("failed to delete series: %v", err)
			}

			keys = e.FileStore.Keys()
			if exp, got := 4, len(keys); exp != got {
				t.Fatalf("series count mismatch: exp %v, got %v", exp, got)
			}

			exp := "cpu,host=B#!~#value"
			if _, ok := keys[exp]; !ok {
				t.Fatalf("wrong series deleted: exp %v, got %v", exp, keys)
			}

			// Deleting all the TSM values for a single series should still leave
			// the series in the index intact.
			indexSet := tsdb.IndexSet{Indexes: []tsdb.Index{e.index}, SeriesFile: e.sfile}
			iter, err := indexSet.MeasurementSeriesIDIterator([]byte("cpu"))
			if err != nil {
				t.Fatalf("iterator error: %v", err)
			} else if iter == nil {
				t.Fatal("nil iterator")
			}
			defer iter.Close()

			var gotKeys []string
			expKeys := []string{"cpu,host=0", "cpu,host=A", "cpu,host=B", "cpu,host=C"}

			for {
				elem, err := iter.Next()
				if err != nil {
					t.Fatal(err)
				}
				if elem.SeriesID == 0 {
					break
				}

				// Lookup series.
				name, tags := e.sfile.Series(elem.SeriesID)
				gotKeys = append(gotKeys, string(models.MakeKey(name, tags)))
			}

			if !reflect.DeepEqual(gotKeys, expKeys) {
				t.Fatalf("got keys %v, expected %v", gotKeys, expKeys)
			}

		})
	}
}

func TestEngine_DeleteSeriesRange_OutsideTime(t *testing.T) {
	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			// Create a few points.
			p1 := MustParsePointString("cpu,host=A value=1.1 1000000000") // Should not be deleted

			e, err := NewEngine(index)
			if err != nil {
				t.Fatal(err)
			}

			// mock the planner so compactions don't run during the test
			e.CompactionPlan = &mockPlanner{}
			if err := e.Open(); err != nil {
				t.Fatal(err)
			}
			defer e.Close()

			for _, p := range []models.Point{p1} {
				if err := e.CreateSeriesIfNotExists(p.Key(), p.Name(), p.Tags()); err != nil {
					t.Fatalf("create series index error: %v", err)
				}
			}

			if err := e.WritePoints([]models.Point{p1}); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			if err := e.WriteSnapshot(); err != nil {
				t.Fatalf("failed to snapshot: %s", err.Error())
			}

			keys := e.FileStore.Keys()
			if exp, got := 1, len(keys); exp != got {
				t.Fatalf("series count mismatch: exp %v, got %v", exp, got)
			}

			itr := &seriesIterator{keys: [][]byte{[]byte("cpu,host=A")}}
			if err := e.DeleteSeriesRange(itr, 0, 0, false); err != nil {
				t.Fatalf("failed to delete series: %v", err)
			}

			keys = e.FileStore.Keys()
			if exp, got := 1, len(keys); exp != got {
				t.Fatalf("series count mismatch: exp %v, got %v", exp, got)
			}

			exp := "cpu,host=A#!~#value"
			if _, ok := keys[exp]; !ok {
				t.Fatalf("wrong series deleted: exp %v, got %v", exp, keys)
			}

			// Check that the series still exists in the index
			iter, err := e.index.MeasurementSeriesIDIterator([]byte("cpu"))
			if err != nil {
				t.Fatalf("iterator error: %v", err)
			}
			defer iter.Close()

			elem, err := iter.Next()
			if err != nil {
				t.Fatal(err)
			}
			if elem.SeriesID == 0 {
				t.Fatalf("series index mismatch: EOF, exp 1 series")
			}

			// Lookup series.
			name, tags := e.sfile.Series(elem.SeriesID)
			if got, exp := name, []byte("cpu"); !bytes.Equal(got, exp) {
				t.Fatalf("series mismatch: got %s, exp %s", got, exp)
			}

			if got, exp := tags, models.NewTags(map[string]string{"host": "A"}); !got.Equal(exp) {
				t.Fatalf("series mismatch: got %s, exp %s", got, exp)
			}
		})
	}
}

func TestEngine_LastModified(t *testing.T) {
	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {
			// Create a few points.
			p1 := MustParsePointString("cpu,host=A value=1.1 1000000000")
			p2 := MustParsePointString("cpu,host=B value=1.2 2000000000")
			p3 := MustParsePointString("cpu,host=A sum=1.3 3000000000")

			e, err := NewEngine(index)
			if err != nil {
				t.Fatal(err)
			}

			// mock the planner so compactions don't run during the test
			e.CompactionPlan = &mockPlanner{}
			e.SetEnabled(false)
			if err := e.Open(); err != nil {
				t.Fatal(err)
			}
			defer e.Close()

			if err := e.WritePoints([]models.Point{p1, p2, p3}); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			lm := e.LastModified()
			if lm.IsZero() {
				t.Fatalf("expected non-zero time, got %v", lm.UTC())
			}
			e.SetEnabled(true)

			if err := e.WriteSnapshot(); err != nil {
				t.Fatalf("failed to snapshot: %s", err.Error())
			}

			lm2 := e.LastModified()

			if got, exp := lm.Equal(lm2), false; exp != got {
				t.Fatalf("expected time change, got %v, exp %v", got, exp)
			}

			itr := &seriesIterator{keys: [][]byte{[]byte("cpu,host=A")}}
			if err := e.DeleteSeriesRange(itr, math.MinInt64, math.MaxInt64, false); err != nil {
				t.Fatalf("failed to delete series: %v", err)
			}

			lm3 := e.LastModified()
			if got, exp := lm2.Equal(lm3), false; exp != got {
				t.Fatalf("expected time change, got %v, exp %v", got, exp)
			}
		})
	}
}

func TestEngine_SnapshotsDisabled(t *testing.T) {
	sfile := MustOpenSeriesFile()
	defer sfile.Close()

	// Generate temporary file.
	dir, _ := ioutil.TempDir("", "tsm")
	walPath := filepath.Join(dir, "wal")
	os.MkdirAll(walPath, 0777)
	defer os.RemoveAll(dir)

	// Create a tsm1 engine.
	db := path.Base(dir)
	opt := tsdb.NewEngineOptions()
	opt.InmemIndex = inmem.NewIndex(db, sfile.SeriesFile)
	idx := tsdb.MustOpenIndex(1, db, filepath.Join(dir, "index"), sfile.SeriesFile, opt)
	defer idx.Close()

	e := tsm1.NewEngine(1, idx, db, dir, walPath, sfile.SeriesFile, opt).(*tsm1.Engine)

	// mock the planner so compactions don't run during the test
	e.CompactionPlan = &mockPlanner{}

	e.SetEnabled(false)
	if err := e.Open(); err != nil {
		t.Fatalf("failed to open tsm1 engine: %s", err.Error())
	}

	// Make sure Snapshots are disabled.
	e.SetCompactionsEnabled(false)
	e.Compactor.DisableSnapshots()

	// Writing a snapshot should not fail when the snapshot is empty
	// even if snapshots are disabled.
	if err := e.WriteSnapshot(); err != nil {
		t.Fatalf("failed to snapshot: %s", err.Error())
	}
}

// Ensure engine can create an ascending cursor for cache and tsm values.
func TestEngine_CreateCursor_Ascending(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {

			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1`,
				`cpu,host=A value=1.2 2`,
				`cpu,host=A value=1.3 3`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			e.MustWriteSnapshot()

			if err := e.WritePointsString(
				`cpu,host=A value=10.1 10`,
				`cpu,host=A value=11.2 11`,
				`cpu,host=A value=12.3 12`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			cur, err := e.CreateCursor(context.Background(), &tsdb.CursorRequest{
				Measurement: "cpu",
				Series:      "cpu,host=A",
				Field:       "value",
				Ascending:   true,
				StartTime:   2,
				EndTime:     11,
			})
			if err != nil {
				t.Fatal(err)
			}

			fcur := cur.(tsdb.FloatBatchCursor)
			ts, vs := fcur.Next()
			if !cmp.Equal([]int64{2, 3, 10, 11}, ts) {
				t.Fatal("unexpect timestamps")
			}
			if !cmp.Equal([]float64{1.2, 1.3, 10.1, 11.2}, vs) {
				t.Fatal("unexpect timestamps")
			}
		})
	}
}

// Ensure engine can create an ascending cursor for tsm values.
func TestEngine_CreateCursor_Descending(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {

			e := MustOpenEngine(index)
			defer e.Close()

			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

			if err := e.WritePointsString(
				`cpu,host=A value=1.1 1`,
				`cpu,host=A value=1.2 2`,
				`cpu,host=A value=1.3 3`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}
			e.MustWriteSnapshot()

			if err := e.WritePointsString(
				`cpu,host=A value=10.1 10`,
				`cpu,host=A value=11.2 11`,
				`cpu,host=A value=12.3 12`,
			); err != nil {
				t.Fatalf("failed to write points: %s", err.Error())
			}

			cur, err := e.CreateCursor(context.Background(), &tsdb.CursorRequest{
				Measurement: "cpu",
				Series:      "cpu,host=A",
				Field:       "value",
				Ascending:   false,
				StartTime:   2,
				EndTime:     11,
			})
			if err != nil {
				t.Fatal(err)
			}

			fcur := cur.(tsdb.FloatBatchCursor)
			ts, vs := fcur.Next()
			if !cmp.Equal([]int64{11, 10, 3, 2}, ts) {
				t.Fatal("unexpect timestamps")
			}
			if !cmp.Equal([]float64{11.2, 10.1, 1.3, 1.2}, vs) {
				t.Fatal("unexpect timestamps")
			}
		})
	}
}

func makeBlockTypeSlice(n int) []byte {
	r := make([]byte, n)
	b := tsm1.BlockFloat64
	m := tsm1.BlockUnsigned + 1
	for i := 0; i < len(r); i++ {
		r[i] = b % m
	}
	return r
}

var blockType = influxql.Unknown

func BenchmarkBlockTypeToInfluxQLDataType(b *testing.B) {
	t := makeBlockTypeSlice(100)
	for i := 0; i < b.N; i++ {
		for j := 0; j < len(t); j++ {
			blockType = tsm1.BlockTypeToInfluxQLDataType(t[j])
		}
	}
}

// This test ensures that "sync: WaitGroup is reused before previous Wait has returned" is
// is not raised.
func TestEngine_DisableEnableCompactions_Concurrent(t *testing.T) {
	t.Parallel()

	for _, index := range tsdb.RegisteredIndexes() {
		t.Run(index, func(t *testing.T) {

			e := MustOpenEngine(index)
			defer e.Close()

			var wg sync.WaitGroup
			wg.Add(2)

			go func() {
				defer wg.Done()
				for i := 0; i < 1000; i++ {
					e.SetCompactionsEnabled(true)
					e.SetCompactionsEnabled(false)
				}
			}()

			go func() {
				defer wg.Done()
				for i := 0; i < 1000; i++ {
					e.SetCompactionsEnabled(false)
					e.SetCompactionsEnabled(true)
				}
			}()
			wg.Wait()
		})
	}
}

func BenchmarkEngine_CreateIterator_Count_1K(b *testing.B) {
	benchmarkEngineCreateIteratorCount(b, 1000)
}
func BenchmarkEngine_CreateIterator_Count_100K(b *testing.B) {
	benchmarkEngineCreateIteratorCount(b, 100000)
}
func BenchmarkEngine_CreateIterator_Count_1M(b *testing.B) {
	benchmarkEngineCreateIteratorCount(b, 1000000)
}

func benchmarkEngineCreateIteratorCount(b *testing.B, pointN int) {
	benchmarkIterator(b, query.IteratorOptions{
		Expr:      influxql.MustParseExpr("count(value)"),
		Ascending: true,
		StartTime: influxql.MinTime,
		EndTime:   influxql.MaxTime,
	}, pointN)
}

func BenchmarkEngine_CreateIterator_First_1K(b *testing.B) {
	benchmarkEngineCreateIteratorFirst(b, 1000)
}
func BenchmarkEngine_CreateIterator_First_100K(b *testing.B) {
	benchmarkEngineCreateIteratorFirst(b, 100000)
}
func BenchmarkEngine_CreateIterator_First_1M(b *testing.B) {
	benchmarkEngineCreateIteratorFirst(b, 1000000)
}

func benchmarkEngineCreateIteratorFirst(b *testing.B, pointN int) {
	benchmarkIterator(b, query.IteratorOptions{
		Expr:       influxql.MustParseExpr("first(value)"),
		Dimensions: []string{"host"},
		Ascending:  true,
		StartTime:  influxql.MinTime,
		EndTime:    influxql.MaxTime,
	}, pointN)
}

func BenchmarkEngine_CreateIterator_Last_1K(b *testing.B) {
	benchmarkEngineCreateIteratorLast(b, 1000)
}
func BenchmarkEngine_CreateIterator_Last_100K(b *testing.B) {
	benchmarkEngineCreateIteratorLast(b, 100000)
}
func BenchmarkEngine_CreateIterator_Last_1M(b *testing.B) {
	benchmarkEngineCreateIteratorLast(b, 1000000)
}

func benchmarkEngineCreateIteratorLast(b *testing.B, pointN int) {
	benchmarkIterator(b, query.IteratorOptions{
		Expr:       influxql.MustParseExpr("last(value)"),
		Dimensions: []string{"host"},
		Ascending:  true,
		StartTime:  influxql.MinTime,
		EndTime:    influxql.MaxTime,
	}, pointN)
}

func BenchmarkEngine_CreateIterator_Limit_1K(b *testing.B) {
	benchmarkEngineCreateIteratorLimit(b, 1000)
}
func BenchmarkEngine_CreateIterator_Limit_100K(b *testing.B) {
	benchmarkEngineCreateIteratorLimit(b, 100000)
}
func BenchmarkEngine_CreateIterator_Limit_1M(b *testing.B) {
	benchmarkEngineCreateIteratorLimit(b, 1000000)
}

func BenchmarkEngine_WritePoints(b *testing.B) {
	batchSizes := []int{10, 100, 1000, 5000, 10000}
	for _, sz := range batchSizes {
		for _, index := range tsdb.RegisteredIndexes() {
			e := MustOpenEngine(index)
			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
			pp := make([]models.Point, 0, sz)
			for i := 0; i < sz; i++ {
				p := MustParsePointString(fmt.Sprintf("cpu,host=%d value=1.2", i))
				pp = append(pp, p)
			}

			b.Run(fmt.Sprintf("%s_%d", index, sz), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					err := e.WritePoints(pp)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
			e.Close()
		}
	}
}

func BenchmarkEngine_WritePoints_Parallel(b *testing.B) {
	batchSizes := []int{1000, 5000, 10000, 25000, 50000, 75000, 100000, 200000}
	for _, sz := range batchSizes {
		for _, index := range tsdb.RegisteredIndexes() {
			e := MustOpenEngine(index)
			e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)

			cpus := runtime.GOMAXPROCS(0)
			pp := make([]models.Point, 0, sz*cpus)
			for i := 0; i < sz*cpus; i++ {
				p := MustParsePointString(fmt.Sprintf("cpu,host=%d value=1.2,other=%di", i, i))
				pp = append(pp, p)
			}

			b.Run(fmt.Sprintf("%s_%d", index, sz), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var wg sync.WaitGroup
					errC := make(chan error)
					for i := 0; i < cpus; i++ {
						wg.Add(1)
						go func(i int) {
							defer wg.Done()
							from, to := i*sz, (i+1)*sz
							err := e.WritePoints(pp[from:to])
							if err != nil {
								errC <- err
								return
							}
						}(i)
					}

					go func() {
						wg.Wait()
						close(errC)
					}()

					for err := range errC {
						if err != nil {
							b.Error(err)
						}
					}
				}
			})
			e.Close()
		}
	}
}

func benchmarkEngineCreateIteratorLimit(b *testing.B, pointN int) {
	benchmarkIterator(b, query.IteratorOptions{
		Expr:       influxql.MustParseExpr("value"),
		Dimensions: []string{"host"},
		Ascending:  true,
		StartTime:  influxql.MinTime,
		EndTime:    influxql.MaxTime,
		Limit:      10,
	}, pointN)
}

func benchmarkIterator(b *testing.B, opt query.IteratorOptions, pointN int) {
	e := MustInitDefaultBenchmarkEngine(pointN)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		itr, err := e.CreateIterator(context.Background(), "cpu", opt)
		if err != nil {
			b.Fatal(err)
		}
		query.DrainIterator(itr)
	}
}

var benchmark struct {
	Engine *Engine
	PointN int
}

var hostNames = []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}

// MustInitDefaultBenchmarkEngine creates a new engine using the default index
// and fills it with points.  Reuses previous engine if the same parameters
// were used.
func MustInitDefaultBenchmarkEngine(pointN int) *Engine {
	// Reuse engine, if available.
	if benchmark.Engine != nil {
		if benchmark.PointN == pointN {
			return benchmark.Engine
		}

		// Otherwise close and remove it.
		benchmark.Engine.Close()
		benchmark.Engine = nil
	}

	const batchSize = 1000
	if pointN%batchSize != 0 {
		panic(fmt.Sprintf("point count (%d) must be a multiple of batch size (%d)", pointN, batchSize))
	}

	e := MustOpenEngine(tsdb.DefaultIndex)

	// Initialize metadata.
	e.MeasurementFields([]byte("cpu")).CreateFieldIfNotExists([]byte("value"), influxql.Float)
	e.CreateSeriesIfNotExists([]byte("cpu,host=A"), []byte("cpu"), models.NewTags(map[string]string{"host": "A"}))

	// Generate time ascending points with jitterred time & value.
	rand := rand.New(rand.NewSource(0))
	for i := 0; i < pointN; i += batchSize {
		var buf bytes.Buffer
		for j := 0; j < batchSize; j++ {
			fmt.Fprintf(&buf, "cpu,host=%s value=%d %d",
				hostNames[j%len(hostNames)],
				100+rand.Intn(50)-25,
				(time.Duration(i+j)*time.Second)+(time.Duration(rand.Intn(500)-250)*time.Millisecond),
			)
			if j != pointN-1 {
				fmt.Fprint(&buf, "\n")
			}
		}

		if err := e.WritePointsString(buf.String()); err != nil {
			panic(err)
		}
	}

	if err := e.WriteSnapshot(); err != nil {
		panic(err)
	}

	// Force garbage collection.
	runtime.GC()

	// Save engine reference for reuse.
	benchmark.Engine = e
	benchmark.PointN = pointN

	return e
}

// Engine is a test wrapper for tsm1.Engine.
type Engine struct {
	*tsm1.Engine
	root  string
	index tsdb.Index
	sfile *tsdb.SeriesFile
}

// NewEngine returns a new instance of Engine at a temporary location.
func NewEngine(index string) (*Engine, error) {
	root, err := ioutil.TempDir("", "tsm1-")
	if err != nil {
		panic(err)
	}

	db := "db0"
	dbPath := filepath.Join(root, "data", db)

	if err := os.MkdirAll(dbPath, os.ModePerm); err != nil {
		return nil, err
	}

	// Setup series file.
	seriesPath, err := ioutil.TempDir(dbPath, tsdb.SeriesFileDirectory)
	if err != nil {
		return nil, err
	}

	sfile := tsdb.NewSeriesFile(seriesPath)
	sfile.Logger = logger.New(os.Stdout)
	if err = sfile.Open(); err != nil {
		return nil, err
	}

	opt := tsdb.NewEngineOptions()
	opt.IndexVersion = index
	if index == "inmem" {
		opt.InmemIndex = inmem.NewIndex(db, sfile)
	}

	idx := tsdb.MustOpenIndex(1, db, filepath.Join(dbPath, "index"), sfile, opt)
	return &Engine{
		Engine: tsm1.NewEngine(1,
			idx,
			db,
			filepath.Join(root, "data"),
			filepath.Join(root, "wal"),
			sfile,
			opt).(*tsm1.Engine),
		root:  root,
		index: idx,
		sfile: sfile,
	}, nil
}

// SeriesFile is a test wrapper for tsdb.SeriesFile.
type SeriesFile struct {
	*tsdb.SeriesFile
}

// NewSeriesFile returns a new instance of SeriesFile with a temporary file path.
func NewSeriesFile() *SeriesFile {
	dir, err := ioutil.TempDir("", "tsdb-series-file-")
	if err != nil {
		panic(err)
	}
	return &SeriesFile{SeriesFile: tsdb.NewSeriesFile(dir)}
}

// MustOpenSeriesFile returns a new, open instance of SeriesFile. Panic on error.
func MustOpenSeriesFile() *SeriesFile {
	f := NewSeriesFile()
	if err := f.Open(); err != nil {
		panic(err)
	}
	return f
}

// Close closes the log file and removes it from disk.
func (f *SeriesFile) Close() {
	defer os.RemoveAll(f.Path())
	if err := f.SeriesFile.Close(); err != nil {
		panic(err)
	}
}

// MustOpenEngine returns a new, open instance of Engine.
func MustOpenEngine(index string) *Engine {
	e, err := NewEngine(index)
	if err != nil {
		panic(err)
	}

	if err := e.Open(); err != nil {
		panic(err)
	}
	return e
}

// Close closes the engine and removes all underlying data.
func (e *Engine) Close() error {
	if e.index != nil {
		e.index.Close()
	}

	if e.sfile != nil {
		e.sfile.Close()
	}

	defer os.RemoveAll(e.root)
	return e.Engine.Close()
}

// Reopen closes and reopens the engine.
func (e *Engine) Reopen() error {
	if err := e.Engine.Close(); err != nil {
		return err
	} else if e.index.Close(); err != nil {
		return err
	}

	db := path.Base(e.root)
	opt := tsdb.NewEngineOptions()
	opt.InmemIndex = inmem.NewIndex(db, e.sfile)

	e.index = tsdb.MustOpenIndex(1, db, filepath.Join(e.root, "data", "index"), e.sfile, opt)

	e.Engine = tsm1.NewEngine(1,
		e.index,
		db,
		filepath.Join(e.root, "data"),
		filepath.Join(e.root, "wal"),
		e.sfile,
		opt).(*tsm1.Engine)

	if err := e.Engine.Open(); err != nil {
		return err
	}
	return nil
}

// MustWriteSnapshot forces a snapshot of the engine. Panic on error.
func (e *Engine) MustWriteSnapshot() {
	if err := e.WriteSnapshot(); err != nil {
		panic(err)
	}
}

// WritePointsString parses a string buffer and writes the points.
func (e *Engine) WritePointsString(buf ...string) error {
	return e.WritePoints(MustParsePointsString(strings.Join(buf, "\n")))
}

// MustParsePointsString parses points from a string. Panic on error.
func MustParsePointsString(buf string) []models.Point {
	a, err := models.ParsePointsString(buf)
	if err != nil {
		panic(err)
	}
	return a
}

// MustParsePointString parses the first point from a string. Panic on error.
func MustParsePointString(buf string) models.Point { return MustParsePointsString(buf)[0] }

type mockPlanner struct{}

func (m *mockPlanner) Plan(lastWrite time.Time) []tsm1.CompactionGroup { return nil }
func (m *mockPlanner) PlanLevel(level int) []tsm1.CompactionGroup      { return nil }
func (m *mockPlanner) PlanOptimize() []tsm1.CompactionGroup            { return nil }
func (m *mockPlanner) Release(groups []tsm1.CompactionGroup)           {}
func (m *mockPlanner) FullyCompacted() bool                            { return false }
func (m *mockPlanner) ForceFull()                                      {}

// ParseTags returns an instance of Tags for a comma-delimited list of key/values.
func ParseTags(s string) query.Tags {
	m := make(map[string]string)
	for _, kv := range strings.Split(s, ",") {
		a := strings.Split(kv, "=")
		m[a[0]] = a[1]
	}
	return query.NewTags(m)
}

type seriesIterator struct {
	keys [][]byte
}

type series struct {
	name    []byte
	tags    models.Tags
	deleted bool
}

func (s series) Name() []byte        { return s.name }
func (s series) Tags() models.Tags   { return s.tags }
func (s series) Deleted() bool       { return s.deleted }
func (s series) Expr() influxql.Expr { return nil }

func (itr *seriesIterator) Close() error { return nil }

func (itr *seriesIterator) Next() (tsdb.SeriesElem, error) {
	if len(itr.keys) == 0 {
		return nil, nil
	}
	name, tags := models.ParseKeyBytes(itr.keys[0])
	s := series{name: name, tags: tags}
	itr.keys = itr.keys[1:]
	return s, nil
}
