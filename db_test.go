package main

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func withMockDB(t *testing.T, fn func(sqlmock.Sqlmock)) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	prev := dbOpen
	dbOpen = func(Config) (*sql.DB, error) { return db, nil }
	dbInstance = nil
	t.Cleanup(func() {
		dbOpen = prev
		dbInstance = nil
		_ = db.Close()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock expectations: %v", err)
		}
	})
	fn(mock)
}

func TestListImageVectorBackfillCandidates_Success(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		rows := sqlmock.NewRows([]string{"id", "imageFile_id", "imageFile_extension"}).
			AddRow(int64(1), "a", "jpg").
			AddRow(int64(2), "b", "")
		mock.ExpectQuery("SELECT").WithArgs(driver.Value(int64(2))).WillReturnRows(rows)

		cfg := Config{QueriedDbTable: "Photo", ImageBucket: "my-bucket"}
		got, err := ListImageVectorBackfillCandidates(cfg, ListImageVectorBackfillCandidatesInput{Mode: "missing", Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ImageBucket != "my-bucket" || got[0].ID != "1" || got[1].ImageFileID != "b" {
			t.Fatalf("unexpected %+v", got)
		}
	})
}

func TestListImageVectorBackfillCandidates_QueryError(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery("SELECT").WithArgs(driver.Value(int64(1))).WillReturnError(errors.New("boom"))

		_, err := ListImageVectorBackfillCandidates(Config{QueriedDbTable: "Photo"}, ListImageVectorBackfillCandidatesInput{Mode: "all", Limit: 1})
		if err == nil || !strings.Contains(err.Error(), "query backfill candidates") {
			t.Fatalf("expected query error, got %v", err)
		}
	})
}

func TestListImageVectorBackfillCandidates_ScanError(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		// Wrong column shape forces Scan to fail
		rows := sqlmock.NewRows([]string{"id"}).AddRow(int64(1))
		mock.ExpectQuery("SELECT").WithArgs(driver.Value(int64(1))).WillReturnRows(rows)

		_, err := ListImageVectorBackfillCandidates(Config{QueriedDbTable: "Photo"}, ListImageVectorBackfillCandidatesInput{Mode: "missing", Limit: 1})
		if err == nil || !strings.Contains(err.Error(), "scan backfill candidate") {
			t.Fatalf("expected scan error, got %v", err)
		}
	})
}

func TestUpdateImageVectorOnly(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(sqlmock.AnyArg(), driver.Value("fid")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := UpdateImageVectorOnly(Config{QueriedDbTable: "Photo"}, "fid", []float64{0.1, 0.2})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateImageLabels(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), driver.Value("fid")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := UpdateImageLabels(
			Config{QueriedDbTable: "Photo"},
			"fid",
			[]ImageLabel{{Description: "Owl", Score: 0.91}},
			[]ImageLabelSuggestion{{Tag: "owl", Label: "Owl", Score: 0.91}},
		)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestMarkImageLabelsFailed(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("vision failed"), driver.Value("fid")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		if err := MarkImageLabelsFailed(Config{QueriedDbTable: "Photo"}, "fid", "vision failed"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateImageVectorOnly_ExecError(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(sqlmock.AnyArg(), driver.Value("fid")).
			WillReturnError(errors.New("exec fail"))

		err := UpdateImageVectorOnly(Config{QueriedDbTable: "Photo"}, "fid", []float64{1})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestMarkImageVectorBackfillAttempt(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("x")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := MarkImageVectorBackfillAttempt(Config{QueriedDbTable: "Photo"}, "x"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMarkImageVectorBackfillFailed(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		ts := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value(ts), driver.Value("reason"), driver.Value("id1")).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := MarkImageVectorBackfillFailed(Config{QueriedDbTable: "Photo"}, "id1", "reason", ts); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateImageMetadata_NoVector(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		selRows := sqlmock.NewRows([]string{"id", "imageFile_id", "imageFile_extension"})
		mock.ExpectQuery(`bit_count\(\('x' \|\| phash\)::bit\(64\) # \('x' \|\| \$2\)::bit\(64\)\) <= 2`).
			WithArgs(driver.Value("img1"), driver.Value("abcd0000")).
			WillReturnRows(selRows)
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("abcd0000"), sqlmock.AnyArg(), sqlmock.AnyArg(), driver.Value("img1")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := UpdateImageMetadata(Config{QueriedDbTable: "Photo"}, "img1", "abcd0000", "bkt", map[string]interface{}{"k": "v"}, nil)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateImageMetadata_WithVector(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		selRows := sqlmock.NewRows([]string{"id", "imageFile_id", "imageFile_extension"}).
			AddRow("dup-1", "other", "png")
		mock.ExpectQuery(`bit_count\(\('x' \|\| phash\)::bit\(64\) # \('x' \|\| \$2\)::bit\(64\)\) <= 2`).
			WithArgs(driver.Value("img1"), driver.Value("abcd0000")).
			WillReturnRows(selRows)
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("abcd0000"), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), driver.Value("img1")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		vec := []float64{0.1, 0.2}
		err := UpdateImageMetadata(Config{QueriedDbTable: "Photo"}, "img1", "abcd0000", "mybucket", map[string]interface{}{}, vec)
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateImageMetadata_QueryFails(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(`SELECT id, "imageFile_id", "imageFile_extension" FROM "Photo"`).
			WithArgs(driver.Value("img1"), driver.Value("abcd0000")).
			WillReturnError(errors.New("no select"))

		err := UpdateImageMetadata(Config{QueriedDbTable: "Photo"}, "img1", "abcd0000", "bkt", nil, nil)
		if err == nil || !regexp.MustCompile(`query similar images`).MatchString(err.Error()) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestUpdateImageMetadata_UpdateFails(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		selRows := sqlmock.NewRows([]string{"id", "imageFile_id", "imageFile_extension"})
		mock.ExpectQuery(`SELECT id, "imageFile_id", "imageFile_extension" FROM "Photo"`).
			WithArgs(driver.Value("img1"), driver.Value("abcd0000")).
			WillReturnRows(selRows)
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("abcd0000"), sqlmock.AnyArg(), sqlmock.AnyArg(), driver.Value("img1")).
			WillReturnError(errors.New("update fail"))

		err := UpdateImageMetadata(Config{QueriedDbTable: "Photo"}, "img1", "abcd0000", "bkt", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "update image metadata") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestListImageVectorBackfillCandidates_DBOpenFail(t *testing.T) {
	prev := dbOpen
	dbOpen = func(Config) (*sql.DB, error) { return nil, errors.New("no db") }
	dbInstance = nil
	t.Cleanup(func() {
		dbOpen = prev
		dbInstance = nil
	})
	_, err := ListImageVectorBackfillCandidates(Config{QueriedDbTable: "Photo"}, ListImageVectorBackfillCandidatesInput{Mode: "all", Limit: 1})
	if err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateImageVectorOnly_DBOpenFail(t *testing.T) {
	prev := dbOpen
	dbOpen = func(Config) (*sql.DB, error) { return nil, errors.New("no db") }
	dbInstance = nil
	t.Cleanup(func() {
		dbOpen = prev
		dbInstance = nil
	})
	err := UpdateImageVectorOnly(Config{QueriedDbTable: "Photo"}, "x", []float64{1})
	if err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("got %v", err)
	}
}

func TestMarkImageVectorBackfillAttempt_ExecError(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("x")).
			WillReturnError(errors.New("exec"))
		if err := MarkImageVectorBackfillAttempt(Config{QueriedDbTable: "Photo"}, "x"); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestMarkImageVectorBackfillFailed_ExecError(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		ts := time.Now()
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value(ts), driver.Value("r"), driver.Value("id")).
			WillReturnError(errors.New("exec"))
		if err := MarkImageVectorBackfillFailed(Config{QueriedDbTable: "Photo"}, "id", "r", ts); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestUpdateImageMetadata_ScanSkipsBadRow(t *testing.T) {
	withMockDB(t, func(mock sqlmock.Sqlmock) {
		selRows := sqlmock.NewRows([]string{"id", "imageFile_id", "imageFile_extension"}).
			AddRow(nil, "other", "png")
		mock.ExpectQuery(`SELECT id, "imageFile_id", "imageFile_extension" FROM "Photo"`).
			WithArgs(driver.Value("img1"), driver.Value("abcd0000")).
			WillReturnRows(selRows)
		mock.ExpectExec(`UPDATE "Photo"`).
			WithArgs(driver.Value("abcd0000"), sqlmock.AnyArg(), sqlmock.AnyArg(), driver.Value("img1")).
			WillReturnResult(sqlmock.NewResult(0, 1))

		err := UpdateImageMetadata(Config{QueriedDbTable: "Photo"}, "img1", "abcd0000", "bkt", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	})
}
