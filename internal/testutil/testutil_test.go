package testutil_test

import (
	"os"
	"testing"

	"anz-training/internal/testutil"
	"anz-training/model"
)

func TestTestKey_Is32BytesAndDeterministic(t *testing.T) {
	if k := testutil.TestKey(7); len(k) != 32 || k[0] != 7 || k[31] != 7 {
		t.Fatalf("unexpected key %x", k)
	}
}

func TestSchemaPath_Exists(t *testing.T) {
	if _, err := os.Stat(testutil.SchemaPath()); err != nil {
		t.Fatal(err)
	}
}

func TestMemStore_AddGetHist(t *testing.T) {
	s := testutil.NewMemStore()
	s.Add(modelProject("p1"))
	if got := s.Get("p1"); got.ID != "p1" {
		t.Fatalf("Get = %+v", got)
	}
	if len(s.Hist()) != 0 {
		t.Fatal("Add must not record history")
	}
}

func modelProject(id string) model.Project { return model.Project{ID: id} }
