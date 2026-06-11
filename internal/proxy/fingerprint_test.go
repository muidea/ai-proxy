package proxy

import (
	"strings"
	"testing"
)

func TestComputeRequestFingerprintDeterministic(t *testing.T) {
	body := []byte(`{"system":"you are a helpful assistant","messages":[{"role":"user","content":"hi"}]}`)
	h1, f1 := ComputeRequestFingerprint(body)
	h2, f2 := ComputeRequestFingerprint(body)
	if h1 != h2 || f1 != f2 {
		t.Fatalf("fingerprint not deterministic: %s/%s vs %s/%s", h1, f1, h2, f2)
	}
	if len(h1) != 64 || len(f1) != 64 {
		t.Fatalf("hash length = %d, want 64", len(h1))
	}
}

func TestComputeRequestFingerprintIgnoresLongContent(t *testing.T) {
	// 两个 body 共享前 stablePrefixBytes 字节,后续内容不同,前缀哈希应相同。
	padding := strings.Repeat("a", 4096)
	short := []byte(`{"system":"sys","messages":[{"role":"user","content":"` + padding + `"}]}`)
	long := []byte(`{"system":"sys","messages":[{"role":"user","content":"` + padding + `"},{"role":"user","content":"extra"}]}`)
	shortHash, _ := ComputeRequestFingerprint(short)
	longHash, _ := ComputeRequestFingerprint(long)
	if shortHash != longHash {
		t.Fatalf("hashes should match when shared prefix bytes are identical: %s vs %s", shortHash, longHash)
	}

	// 反向验证:body 主体内容差异落在前 256 字节内时,哈希应不同。
	different := []byte(`{"system":"sys","messages":[{"role":"user","content":"different content here"}]}`)
	diffHash, _ := ComputeRequestFingerprint(different)
	if diffHash == shortHash {
		t.Fatalf("hashes should differ when leading bytes differ")
	}
}

func TestComputeRequestFingerprintNonJSONFallback(t *testing.T) {
	body := []byte("not json at all")
	h, f := ComputeRequestFingerprint(body)
	if h == "" || f == "" {
		t.Fatalf("non-JSON body should still produce hash: %s/%s", h, f)
	}
	if h != f {
		t.Fatalf("non-JSON fallback should yield identical prefix and full hash: %s vs %s", h, f)
	}
}

func TestFingerprintDriftTrackerFirstObservationBaseline(t *testing.T) {
	d := NewFingerprintDriftTracker(2)
	drift, count := d.Observe("hash-1")
	if drift {
		t.Fatalf("first observation should not be drift")
	}
	if count != 0 {
		t.Fatalf("consec = %d, want 0", count)
	}
}

func TestFingerprintDriftTrackerFiresAfterThreshold(t *testing.T) {
	d := NewFingerprintDriftTracker(2)
	d.Observe("hash-1")
	// consec=1
	drift, count := d.Observe("hash-2")
	if drift {
		t.Fatalf("after 1 change should not fire, got drift=true")
	}
	if count != 1 {
		t.Fatalf("consec = %d, want 1", count)
	}
	drift, count = d.Observe("hash-3")
	if !drift {
		t.Fatalf("after 2 changes should fire, got drift=false")
	}
	if count != 2 {
		t.Fatalf("consec = %d, want 2", count)
	}
}

func TestFingerprintDriftTrackerResetsOnSameHash(t *testing.T) {
	d := NewFingerprintDriftTracker(2)
	d.Observe("a")
	d.Observe("b") // consec=1
	d.Observe("a") // resets
	drift, _ := d.Observe("c")
	if drift {
		t.Fatalf("consecutive should reset to 1, not fire")
	}
}

func TestFingerprintDriftTrackerInvalidThreshold(t *testing.T) {
	d := NewFingerprintDriftTracker(0)
	if d.threshold != 1 {
		t.Fatalf("invalid threshold should default to 1, got %d", d.threshold)
	}
}

func TestFingerprintDriftTrackerNilSafe(t *testing.T) {
	var d *FingerprintDriftTracker
	drift, _ := d.Observe("any")
	if drift {
		t.Fatalf("nil tracker must not fire")
	}
	d.Reset()
}
