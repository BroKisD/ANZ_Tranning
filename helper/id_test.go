package helper_test

import (
	"regexp"
	"testing"

	"anz-training/helper"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewID_Format(t *testing.T) {
	id := helper.NewID()
	if !uuidV4.MatchString(id) {
		t.Fatalf("%q is not a v4 UUID", id)
	}
}

func TestNewID_Unique(t *testing.T) {
	if helper.NewID() == helper.NewID() {
		t.Fatal("two calls returned the same id")
	}
}
