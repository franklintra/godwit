package rpcbind

import (
	"context"

	"github.com/godwit-email/godwit/core/store"
	"github.com/godwit-email/godwit/rpc"
)

// interactiveMethods are the interface's calls the user waits on now:
// opening a mail, switching a view, composing, and the clicks that change
// a mail (their writes go to the writer anyway; their reads come first).
// Their plain SELECTs run on the interactive readers (store.Interactive,
// ADR 0052). Every other call of the ui role is store.UIBackground:
// badges, counts, avatars, saved views. Pinned in
// testdata/interactive_methods.txt (TestInteractiveMethodsPinned); edit
// both as data.
var interactiveMethods = map[string]bool{
	// Opening a mail.
	"GetThread": true, "GetMessage": true, "GetMessageLazyImages": true, "InlineImagesStream": true,
	"ThreadDeadline": true, "ThreadLabels": true, "ImagesAllowed": true, "TrustedImageSenders": true,
	"UnsubscribeMethod": true, "PluginCards": true, "PluginTags": true, "CachedAttachments": true,
	"SummaryArrivalState": true, "MessageLabels": true, "OpenPGPMessage": true,
	// Switching a view, searching.
	"ListThreads": true, "ListMessages": true, "ViewContents": true, "ViewGroupPage": true, "Search": true,
	// What the window and compose read for their frame.
	"ListAccounts": true, "ListIdentities": true, "ListSignatures": true, "ListLabels": true,
	"AllAccountLabels": true, "GetSettings": true, "LabelModelReady": true, "TaskModel": true,
	"ModelName": true, "AccountIcon": true, "ListAddressBooks": true, "SetUIForeground": true,
	// Compose.
	"NewDraft": true, "NewMailtoDraft": true, "OpenDraft": true, "GetDraft": true, "SaveDraft": true,
	"DeleteDraftIfEmpty": true, "AddAttachment": true, "AddAttachmentAt": true, "RemoveAttachment": true,
	"RemoveAttachmentAt": true, "SetDraftAccount": true, "SetDraftFrom": true, "ListOutbox": true,
	"Find": true, "SignatureShown": true,
	// Clicks that change a mail.
	"SetFlags": true, "Move": true, "MoveFrom": true, "MoveToRole": true, "ArchiveThreads": true,
	"ArchiveMessages": true, "Delete": true, "DeleteConversation": true, "SetLabel": true,
}

// laneFor is a call's lane: by the connection's role and the method, then
// lowered (never raised) by the request's lane hint. Only the ui role is
// ever above store.Background: full and mcp clients are the user's
// programs, whose work must not take the interface's connections.
func laneFor(role, method, hint string) store.Lane {
	lane := store.Background
	if role == rpc.RoleUI {
		lane = store.UIBackground
		if interactiveMethods[method] {
			lane = store.Interactive
		}
	}
	if h, ok := store.ParseLane(hint); ok && h < lane {
		lane = h
	}
	return lane
}

// callContext marks a call's ctx with its lane (rpc.ServerOptions.CallContext).
func callContext(ctx context.Context, role, method, hint string) context.Context {
	return store.WithLane(ctx, laneFor(role, method, hint), method)
}
