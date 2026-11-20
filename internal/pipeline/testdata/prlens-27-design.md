### Scope

Let the user edit (1) their private notes, (2) AI notes, and (3) any note, private or AI, that was promoted into the user's still-PENDING GitHub review, so that saving the edit also updates the GitHub review comment. Comments that were promoted before this change have no saved GitHub comment ID; for those, only the local note text is edited, which the requester said is fine. Comments in a review that has already been submitted are not edited on GitHub.

### Architecture

Data (src/shared/notes.ts): `NotesFile` gains two fields that default to empty, so existing files still parse. `edits: record(NoteId, {body})` holds the user's replacement body for AI notes; the cached analysis stays unchanged, and since AI note IDs are stable hashes the edit survives re-runs. `published: record(NoteId, {commentId, body})` links a note to its GitHub pending-review comment and keeps the text that was last posted. Only main writes `published`; the renderer has no channel that can set it, and the existing `NoteState` patch channel is unchanged. `PrivateNote.body` max goes from 10,000 to 65,000, matching the comment limit, so posted text can be copied into the note; this only widens the limit, so existing files still load.

Notes service (src/main/notes/notes.ts), using the existing serialized `mutate`: `updateBody(subject, id, body)` for private notes, `setEdit(subject, id, body)` for AI notes, `setPublished(subject, id, link | null)`, `clearPublished(subject)`. `delete` also drops the note's `edits` and `published` entries.

Write client (src/main/promote/write-client.ts): the ADD_*_THREAD mutations also select the comment `id`, and `addThread` returns `{url, commentId}`. New `pendingComment(commentId)` query: `node(id){... on PullRequestReviewComment{ state author{login} viewerCanUpdate pullRequest{number repository{owner{login} name}} }}` plus `viewer{login}`. New `updateComment(commentId, body)` uses `updatePullRequestReviewComment` with the body sent through `-f`, like every other value. write-client.ts is still imported only by promote.ts.

Promote service (src/main/promote/promote.ts):
- `confirm`: after `addThread`, calls `setPublished(subject, noteId, {commentId, body: text})`. For a private note it also sets the note's body to the posted text, so note and comment match, including any changes made in the dialog.
- `submitReview`: on success, calls `clearPublished(subject)`. The renderer already clears `promoted`.
- New `editNote(targetKey, noteId, body)`, using the target already loaded in main (as promote does): the body is trimmed and must not be empty. If the note has no `published` link, it saves locally (private: `updateBody`, up to 10,000 characters for unpromoted notes as today; AI: `setEdit`) and returns `{synced: 'local'}`. If it has a link: `requireEnabled()`; `requirePr`; `pendingComment` must report state PENDING, author == viewer, `viewerCanUpdate`, and the same owner/repo/number as the loaded PR. If all pass, it calls `updateComment`, then updates `published.body` (and the private note's body), and returns `{synced: 'github'}`. If any check fails, or GitHub reports NOT_FOUND, it clears the link and `promoted`, saves the text locally, and returns `{synced: 'local', reason}`. Any other error (auth, network) is thrown and nothing is saved, so local text and GitHub don't silently drift apart. The head-SHA check is not needed, because editing a comment's body doesn't change its anchor.

IPC: new channel `notes:edit` with input `{targetKey: TargetKey, id: NoteId, body: string 1..65000}` (strict) and output `{notes: NotesFile, synced: 'github' | 'local', reason?: string}`. It is registered in register.ts as `s.promote.editNote` and exposed in preload as `notes.edit`.

Renderer:
- `toViewNotes` takes `edits` and `published`. A linked note shows the posted text (`published.body`); an AI note with an edit shows its edited body with an 'Edited' tag. The AI title and question are unchanged.
- NoteCard gets an Edit button, which turns the body into a textarea in the same style as the Composer (Cmd/Ctrl+Enter saves, Esc cancels). It is pre-filled with the posted text for linked notes and the shown body otherwise. While a note is promoted, its label reads 'Saving updates your pending GitHub comment', and Edit is disabled with a reason if the setting is off.
- `useReview.editNote` calls `notes:edit`, replaces `notes` with the returned file, and shows a notice when `synced === 'local'` but a reason is given.
- `defaultBody` uses an AI note's edited body when there is one.
- Keyboard shortcuts already ignore textareas.

### Alternatives considered

Using the confirm-token preview flow for edits as well: rejected. Pending comments are visible only to the viewer, and the edit is the user's own explicit save and can be edited again. The setting check, the ownership check and the PENDING check still run on every edit. A person approving this design may want to reverse that choice.

Storing the comment ID in `NoteState`: rejected, because the renderer can patch `NoteState` freely and could then aim an edit at a different comment. Keeping it in a field only main writes, and checking it against GitHub before every update, is safer.

Updating local text first and syncing to GitHub later: rejected, because it lets the note and the comment drift apart silently.

Rewriting the AI note inside the cached analysis: rejected, because a re-run would overwrite it and it would break the cache's 'paid-for review' semantics.

Finding comments promoted earlier by body text: not needed; the requester accepted that they stay local.

### Constraints

Follow the IPC contract rules: every payload goes through a strict zod schema, the sender frame is checked, and the call returns the typed Result. `gh api graphql` is called with a fixed query and values passed through `-f`. write-client.ts stays imported only by promote.ts. Every write to GitHub is gated by `promoteEnabled`. No publicly visible GitHub content is changed: only comments in the PENDING state are updated. Notes-file writes stay serialized per subject. Older notes files must still load (new fields default to empty). The renderer stays sandboxed and the preload stays a fixed method list. Use the existing ds/ classes; no new dependencies.

### Implementation plan

1. shared/notes.ts: add the `NoteEdit` and `PublishedLink` schemas, the `edits` and `published` fields (defaulting to empty) on `NotesFile`, raise the `PrivateNote.body` max to 65,000, and add `EditNoteInput`.
2. shared/ipc.ts: add the `notes:edit` contract entry and the result type.
3. main/notes/notes.ts: add `updateBody`, `setEdit`, `setPublished`, `clearPublished`; make `delete` drop the matching entries.
4. main/notes/resolve.ts: return the edited body for AI notes, and expose `published` to callers.
5. main/promote/write-client.ts: select the comment `id` in the thread mutations; add the `pendingComment` query and the `updateComment` mutation with zod parsing; map 'Could not resolve to a node' to NOT_FOUND in `classifyWriteError`.
6. main/promote/promote.ts: save the link (and the private note body) in `confirm`; clear links in `submitReview`; add `editNote` with the decision tree described in the architecture; make `defaultBody` use the AI edit.
7. main/ipc/register.ts and preload/index.ts (plus index.d.ts if needed): wire up `notes:edit`.
8. renderer: model.ts `toViewNotes` with edits and links; NoteView.tsx Edit mode and labels; useReview.ts `editNote`; ReviewPage.tsx passes the handler and the reason Edit is disabled (setting off while the note is promoted).
9. ARCHITECTURE.md: document editing in 'Promote to comment' and 'Storage', and update the write-client sentence to 'creates, extends, or edits the viewer's PENDING review'.

### Tests

New main/notes/notes.test.ts, using the in-memory/temp JsonStore pattern from the existing tests: updateBody, setEdit and setPublished persist; delete removes edits and links; an older file without the new fields parses; concurrent mutations don't lose updates.

promote.test.ts:
- `confirm` saves `{commentId, body}` and, for private notes, the posted body.
- `submitReview` clears links.
- `editNote` with no link saves locally and makes no gh call, including on a compare subject.
- `editNote` with a link and setting off throws and writes nothing.
- `editNote` with a link and a pending comment owned by the viewer on the same PR calls `updateComment` with the exact body and updates the link and the note.
- If the comment is SUBMITTED, has another author, is on a different PR, or is NOT_FOUND, the link and `promoted` are cleared, the note is saved locally and `synced: 'local'` is returned with a reason.
- An auth or network error writes nothing.
- A body starting with `@` is sent with `-f`.

write-client tests (alongside gh.test.ts style): argument lists for `updateComment` and `pendingComment`; parsing of the comment id from `addThread`.

misc/IPC schema tests: `notes:edit` rejects unknown keys, empty or oversized bodies, and malformed ids.

Renderer model.test.ts: `toViewNotes` uses the linked body first, then the edited AI body, then the original.

All existing tests and typecheck/lint must pass in CI.

### Security considerations

No GitHub comment ID ever comes from the renderer: main reads it from the notes file it alone writes. Before every update, main checks with GitHub that the comment is PENDING, written by the gh viewer, editable by the viewer, and on the PR that is loaded. A tampered notes file therefore still cannot edit someone else's comment or a submitted one. Edits to GitHub require `promoteEnabled`, as all writes do. Bodies go through `-f`, never as a shell string, and the 1–65,000 limit is enforced by zod and again in the service. Only pending, viewer-only content is mutated, which keeps the documented guarantee that nothing others can see changes without an explicit submit. Private notes and AI notes that are not linked never leave the machine. Errors are logged by name only, as now.

### Out of scope

Editing comments in reviews that have been submitted, or comments written on github.com or shown in the GitHub context panel. Recovering links for comments promoted before this change. Deleting the GitHub comment when its note is deleted (the comment stays in the pending review; this could be a follow-up). Editing AI note titles, questions, or note anchors and lines. Showing edit history.
