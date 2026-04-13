package stream

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveViewerToken_Empty(t *testing.T) {
	in := "line1\n#EXT-X-MAP:URI=\"init.mp4" + vtPlaceholder + "\"\nsegment_0.m4s" + vtPlaceholder + "\n"
	out := ResolveViewerToken(in, "")
	assert.Equal(t, "line1\n#EXT-X-MAP:URI=\"init.mp4\"\nsegment_0.m4s\n", out)
}

func TestResolveViewerToken_NonEmpty(t *testing.T) {
	in := "#EXT-X-MAP:URI=\"init.mp4" + vtPlaceholder + "\"\nsegment_0.m4s" + vtPlaceholder + "\n"
	out := ResolveViewerToken(in, "ABC123")
	assert.Equal(t, "#EXT-X-MAP:URI=\"init.mp4?vt=ABC123\"\nsegment_0.m4s?vt=ABC123\n", out)
}

func TestResolveViewerToken_EscapesUnsafeChars(t *testing.T) {
	// Well-formed base64url tokens never contain these chars, but defensive
	// escaping must still cover them so handlers cannot accidentally emit
	// an unescaped query fragment.
	out := ResolveViewerToken("segment_0.m4s"+vtPlaceholder+"\n", "a b&c")
	// url.QueryEscape turns ' ' into '+' and '&' into %26.
	assert.Contains(t, out, "?vt=a+b%26c")
}

func TestResolveViewerToken_EmptyPlaylist(t *testing.T) {
	assert.Equal(t, "", ResolveViewerToken("", "xxx"))
}

func TestResolveViewerToken_IdempotentOnResolvedPlaylist(t *testing.T) {
	// A playlist with no placeholders (already resolved) is returned unchanged.
	in := "segment_0.m4s?vt=abc\n"
	out := ResolveViewerToken(in, "xyz")
	assert.Equal(t, in, out, "already-resolved playlist must not be mutated")
}

func TestRenderedPlaylist_PlaceholderAppearsAtEveryURI(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)
	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// One placeholder in EXT-X-MAP and one per segment.
	assert.Equal(t, 4, strings.Count(playlist, vtPlaceholder))
}
