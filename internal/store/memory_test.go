package store_test

import (
	"testing"

	"github.com/zeussilver/tradedoc/internal/store"
	"github.com/zeussilver/tradedoc/internal/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}
