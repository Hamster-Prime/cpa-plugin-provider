package ui

import (
	"bytes"
	"testing"
)

func TestProviderPageSupportsAuthenticatedManagementCenterEmbedding(t *testing.T) {
	for _, marker := range [][]byte{
		[]byte("cpa-multi-protocol-provider-ready"),
		[]byte("cpa-management-auth"),
		[]byte("parent-origin"),
		[]byte("event.source !== window.parent"),
		[]byte("event.origin !== parentOrigin"),
	} {
		if !bytes.Contains(ProviderPage, marker) {
			t.Fatalf("embedded provider page is missing marker %q", marker)
		}
	}
}
