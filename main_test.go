package kome_test

import (
	"os"
	"testing"

	"github.com/intentd/kome/testutil"
	"github.com/intentd/tools/condition"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.Run(m, testutil.OldCRD(nil), testutil.NewCRD()))
}

func whenEquals(path, value string) condition.Conditions {
	return condition.Conditions{{Field: path, Equals: value}}
}
