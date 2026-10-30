package memstore_test

import (
	"testing"

	"github.com/everdict/herd/store/memstore"
	"github.com/everdict/herd/store/storetest"
)

func TestContract(t *testing.T) { storetest.Run(t, memstore.New()) }
