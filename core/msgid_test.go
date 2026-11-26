package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeMessageID(t *testing.T) {
	for in, want := range map[string]string{
		"<a.b@host.example>":         "a.b@host.example",
		"a.b@host.example":           "a.b@host.example",
		"  <a.b@host.example>\r\n":   "a.b@host.example",
		"mid:a.b@host.example":       "a.b@host.example",
		"MID:a%2Bb%40host.example":   "a+b@host.example",
		"mid:a.b@host.example/part1": "a.b@host.example",
		"<>":                         "",
		"   ":                        "",
		"CaseKept@Host":              "CaseKept@Host",
	} {
		assert.Equal(t, want, NormalizeMessageID(in), in)
	}
}

func TestBracketedMessageID(t *testing.T) {
	for in, want := range map[string]string{"<a@b>": "a@b", " <a.b+c@d.example> ": "a.b+c@d.example"} {
		got, ok := bracketedMessageID(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"a@b", "<ab>", "<a@b> invoice", "find <a@b>", "<a @b>", "<a<b@c>", "<>", ""} {
		_, ok := bracketedMessageID(in)
		assert.False(t, ok, in)
	}
}

// One Message-ID, every account that has it, with or without its brackets;
// Search finds it pasted as a header shows it, and by SearchQuery.MessageID.
func TestFindMessagesByMessageID(t *testing.T) {
	e := newSyncEnv(t)
	e2 := e.second(t)
	e.put(t, "INBOX", eml("plan-1@x", "bob@example.com", "alice@example.com", "Plan", "Mon, 05 Oct 2026 09:00:00 +0000", "the plan"))
	e.put(t, "INBOX", eml("other@x", "bob@example.com", "alice@example.com", "Other", "Mon, 05 Oct 2026 10:00:00 +0000", "plan-1@x is quoted here"))
	e2.put(t, "INBOX", eml("plan-1@x", "bob@example.com", "carol@example.com", "Plan", "Mon, 05 Oct 2026 09:00:00 +0000", "the plan"))
	e.sync(t)
	e2.sync(t)

	for _, q := range []string{"plan-1@x", "<plan-1@x>", " <plan-1@x> ", "mid:plan-1%40x"} {
		refs, err := e.s.FindMessagesByMessageID(bg, q)
		require.NoError(t, err, q)
		require.Len(t, refs, 2, q)
		accounts := map[int64]bool{}
		for _, r := range refs {
			accounts[r.AccountID] = true
			assert.Equal(t, "plan-1@x", r.MessageID)
			assert.Equal(t, "Plan", r.Subject)
			assert.NotZero(t, r.ThreadID)
			assert.NotZero(t, r.FolderID, "the folder it shows in")
		}
		assert.Equal(t, map[int64]bool{e.acc.ID: true, e2.acc.ID: true}, accounts, q)
	}
	refs, err := e.s.FindMessagesByMessageID(bg, "PLAN-1@x")
	require.NoError(t, err)
	assert.Empty(t, refs, "a Message-ID is matched exactly")
	_, err = e.s.FindMessagesByMessageID(bg, " <> ")
	assert.Error(t, err)

	got, err := e.s.Search(bg, 0, "<plan-1@x>", 10)
	require.NoError(t, err)
	assert.Len(t, got, 2, "a pasted Message-ID finds that message, not mail quoting it")
	got, err = e.s.Search(bg, e.acc.ID, "<plan-1@x>", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Plan", got[0].Subject)
	got, err = e.s.SearchWith(bg, SearchQuery{MessageID: "other@x"}, 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "a filter needs no text")
	assert.Equal(t, "Other", got[0].Subject)
	got, err = e.s.Search(bg, e.acc.ID, "plan-1@x", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, got, "without brackets it is words, as before")
}
