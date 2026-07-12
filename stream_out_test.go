package gzip

// Streaming-output tests: prove the Writer works as an incremental gzip
// stream producer — the shape an HTTP/SSE compressor needs. Three angles:
//
//  1. after every Flush, the bytes emitted so far decode to exactly
//     everything written so far (Z_SYNC_FLUSH semantics);
//  2. a chunked Write+Flush sequence produces output byte-identical to real
//     C zlib driven with the identical call sequence;
//  3. an end-to-end httptest round trip: the server compresses and flushes
//     chunk by chunk, the client decodes each chunk incrementally with the
//     standard library reader before the response has finished.

import (
	"bytes"
	stdgzip "compress/gzip"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStreamingFlushIncrementalDecode writes chunks with a Flush after each
// and verifies, at every step, that the bytes emitted SO FAR decode to
// exactly the bytes written so far — data is never stuck in the compressor
// after a Flush, at any level.
func TestStreamingFlushIncrementalDecode(t *testing.T) {
	rng := rand.New(rand.NewSource(20260712))
	for _, level := range []int{0, 1, 6, 9} {
		var out bytes.Buffer
		w, err := NewWriterLevel(&out, level)
		if err != nil {
			t.Fatal(err)
		}

		var written bytes.Buffer
		for i := 0; i < 12; i++ {
			chunk := make([]byte, 1+rng.Intn(30000))
			alpha := []int{4, 256}[i%2]
			for j := range chunk {
				chunk[j] = byte(rng.Intn(alpha))
			}
			if _, err := w.Write(chunk); err != nil {
				t.Fatal(err)
			}
			written.Write(chunk)
			before := out.Len()
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if out.Len() == before {
				t.Fatalf("level %d chunk %d: Flush emitted no bytes", level, i)
			}

			// The stream is unfinished, so decode the partial bytes and
			// require exactly everything written so far
			r, err := stdgzip.NewReader(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatalf("level %d chunk %d: %v", level, i, err)
			}
			r.Multistream(false)
			got := make([]byte, written.Len())
			if _, err := io.ReadFull(r, got); err != nil {
				t.Fatalf("level %d chunk %d: partial decode: %v", level, i, err)
			}
			if !bytes.Equal(got, written.Bytes()) {
				t.Fatalf("level %d chunk %d: partial decode mismatch", level, i)
			}
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := stdgzip.NewReader(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, written.Bytes()) {
			t.Fatalf("level %d: final decode mismatch", level)
		}
	}
}

// TestStreamingSequenceMatchesCReference drives the Writer with random
// multi-chunk Write+Flush sequences and requires byte identity with real
// C zlib fed the identical deflate call sequence. The Writer's calls are
// deflate(chunk, NO_FLUSH) per Write, deflate(nil, SYNC) per Flush and
// deflate(nil, FINISH) on Close — expressed as referee stream ops
// (0@len 2@0)... 4@0, framed with the same gzip header/trailer.
func TestStreamingSequenceMatchesCReference(t *testing.T) {
	bin := needRef(t)
	rng := rand.New(rand.NewSource(20260713))
	const mtime = uint32(1751038273)

	for _, level := range []int{1, 6, 9} {
		for run := 0; run < 4; run++ {
			nChunks := 2 + rng.Intn(6)
			var data bytes.Buffer
			var chunkLens []int
			for i := 0; i < nChunks; i++ {
				chunk := make([]byte, rng.Intn(60000))
				alpha := []int{2, 16, 256}[rng.Intn(3)]
				for j := range chunk {
					chunk[j] = byte(rng.Intn(alpha))
				}
				data.Write(chunk)
				chunkLens = append(chunkLens, len(chunk))
			}

			// Writer side: Write+Flush per chunk, then Close
			var out bytes.Buffer
			w, err := NewWriterLevel(&out, level)
			if err != nil {
				t.Fatal(err)
			}
			w.Mtime = mtime
			payload := data.Bytes()
			off := 0
			for _, n := range chunkLens {
				if _, err := w.Write(payload[off : off+n]); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				off += n
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			// C side: identical call sequence via the referee's stream mode
			var chunks, flushes []int
			for _, n := range chunkLens {
				chunks = append(chunks, n, 0)
				flushes = append(flushes, 0, 2)
			}
			chunks = append(chunks, 0)
			flushes = append(flushes, 4)
			raw := refStream(t, bin, payload, level, chunks, flushes)
			want := frameGzip(raw, payload, mtime)

			if !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("level %d run %d (%d chunks): Writer stream differs from C zlib same-sequence output (%d vs %d, first diff@%d)",
					level, run, nChunks, out.Len(), len(want), firstDiff(out.Bytes(), want))
			}
		}
	}
}

// frameGzip wraps raw deflate bytes in this library's gzip framing
// (MTIME=mtime, XFL=0, OS=3) for comparison against Writer output.
func frameGzip(raw, payload []byte, mtime uint32) []byte {
	out := make([]byte, 0, 18+len(raw))
	out = append(out, 0x1f, 0x8b, 0x08, 0x00,
		byte(mtime), byte(mtime>>8), byte(mtime>>16), byte(mtime>>24), 0x00, 0x03)
	out = append(out, raw...)
	crc := crc32.ChecksumIEEE(payload)
	n := uint32(len(payload))
	return append(out,
		byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24),
		byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
}

// TestHTTPStreamingGzip is the end-to-end HTTP shape: the handler streams
// gzip-compressed chunks with Writer.Flush + http.Flusher, and the client
// decodes every chunk incrementally with the standard library reader while
// the response is still open. Server and client run in lockstep (the server
// waits for an ack before the next chunk), so a chunk that stays stuck in a
// buffer deadlocks the test rather than passing by accident.
func TestHTTPStreamingGzip(t *testing.T) {
	messages := make([]string, 8)
	for i := range messages {
		messages[i] = fmt.Sprintf(`{"event":%d,"data":"%s"}`+"\n", i, string(bytes.Repeat([]byte{byte('a' + i)}, 100+i*37)))
	}
	acks := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		fl, ok := rw.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter is not a Flusher")
			return
		}
		rw.Header().Set("Content-Encoding", "gzip")
		rw.Header().Set("Content-Type", "application/x-ndjson")
		gz := NewWriter(rw)
		defer gz.Close()
		for _, msg := range messages {
			if _, err := gz.Write([]byte(msg)); err != nil {
				t.Errorf("server write: %v", err)
				return
			}
			if err := gz.Flush(); err != nil { // make the chunk decodable now
				t.Errorf("server flush: %v", err)
				return
			}
			fl.Flush() // push it onto the wire
			select {
			case <-acks: // client decoded this chunk; send the next
			case <-time.After(10 * time.Second):
				t.Error("server: timed out waiting for client ack")
				return
			}
		}
	}))
	defer srv.Close()

	// Set Accept-Encoding explicitly: otherwise Go's Transport silently
	// decompresses the body itself and this test would not see the raw
	// gzip bytes it needs to decode incrementally
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}

	r, err := stdgzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for i, msg := range messages {
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if string(got) != msg {
			t.Fatalf("chunk %d: got %q want %q", i, got, msg)
		}
		acks <- struct{}{} // only now may the server produce the next chunk
	}
	if rest, err := io.ReadAll(r); err != nil || len(rest) != 0 {
		t.Fatalf("trailing data %d bytes, err %v", len(rest), err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
