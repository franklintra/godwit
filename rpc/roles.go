package rpc

import "encoding/json"

// Roles a connection declares in hello.
const (
	RoleUI   = "ui"   // every exposed method plus UI-only methods (rpcbind/testdata/ui_methods.txt), for Godwit's own interface
	RoleFull = "full" // every method exposed to external programs (rpcbind/testdata/full_methods.txt), for the user's own programs
	RoleMCP  = "mcp"  // read, draft and request a send, for godwit mcp
)

// mcpMethods is the mcp role's allowlist: reads that touch only stored data
// (including SearchContacts over local mail and cached address-book contacts,
// with no binary avatars or network requests, and the views: their stored
// definitions, conversations and why each is there, marking nothing read),
// drafts (their own header fields included, which core holds to
// mail.CheckHeaders: none that sets addresses, threading, the date or the
// content, nor one that redirects replies, receipts or bounces, so a draft
// still goes only where its recipients say; SetDraftFrom picks the account
// and 发件身份 a draft is sent as, as NewDraft picks the account;
// ListIdentities reads the stored ones), and RequestMCPSend, whose
// outcome core decides by the user's 通过 MCP 发送 setting. Nothing else
// sends, and nothing here deletes, changes accounts, address books, settings,
// extensions or model services, decides approvals or downloads attachments.
// SetThreadDeadline sets or clears one conversation's 期限 on this device
// only, as the program source: it sends nothing, changes no mail, and cannot
// replace or clear one the user set (core checks the message it cites).
// FindMessagesByMessageID is a read of stored data. OpenDraft and OpenMessage
// put a draft or a message the user already has on the user's screen in
// Godwit (starting its window if none is open) for the user to review: they
// return nothing of it, and send or change nothing.
var mcpMethods = map[string]bool{
	"ListAccounts": true, "ListFolders": true, "ListThreads": true, "GetThread": true,
	"ListMessages": true, "GetMessageText": true, "Search": true, "SearchContacts": true, "FindMessagesByMessageID": true,
	"ListDrafts": true, "GetDraft": true, "NewDraft": true, "SaveDraft": true, "OpenDraft": true, "OpenMessage": true,
	"ListIdentities": true, "SetDraftFrom": true,
	"SavedViews": true, "ViewContents": true, "ViewGroupPage": true,
	"RequestMCPSend": true, "SetThreadDeadline": true,
}

// mcpDraftModes are the NewDraft modes the mcp role may use; forward is not
// one, since it copies (and may download) the source's attachments.
var mcpDraftModes = map[string]bool{"new": true, "reply": true, "reply_all": true}

// MCPAllowed reports whether the mcp role may call method with params.
func MCPAllowed(method string, params []json.RawMessage) bool {
	if !mcpMethods[method] {
		return false
	}
	if method == "NewDraft" {
		var mode string
		return len(params) >= 2 && json.Unmarshal(params[1], &mode) == nil && mcpDraftModes[mode]
	}
	return true
}
