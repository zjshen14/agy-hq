package kernel

import (
	"os"
	"strings"
)

// maxOwnerWalkHops bounds the nested-subagent parent walk (the same bound AttachWorktreeToTaskWithOptions uses).
const maxOwnerWalkHops = 8

func findOwningTaskIndex(tasks []ActiveTask, nestedMap map[string]NestedConversation, deskConv, cur string) int {
	walkCur := cur
	visited := make(map[string]bool)
	for hop := 0; hop < maxOwnerWalkHops && walkCur != ""; hop++ {
		if visited[walkCur] || (deskConv != "" && walkCur == deskConv) {
			return -1
		}
		visited[walkCur] = true
		for i := range tasks {
			if tasks[i].TaskSlug == walkCur || (tasks[i].ConversationID != "" && tasks[i].ConversationID == walkCur) {
				if deskConv != "" && strings.TrimSpace(tasks[i].ConversationID) == deskConv {
					return -1
				}
				return i
			}
		}
		if nestedMap == nil {
			return -1
		}
		nc, ok := nestedMap[walkCur]
		if !ok || strings.TrimSpace(nc.ParentConversationID) == "" {
			return -1
		}
		walkCur = strings.TrimSpace(nc.ParentConversationID)
	}
	return -1
}

// ResolveOwningTask returns the Layer-1 Task Board entry that owns convOrTaskID:
// a direct TaskSlug/ConversationID match, else walk nested-subagent parent links
// (<= 8 hops, cycle-safe), never returning or walking through the Front Desk conversation.
//
// An empty convOrTaskID falls back to $ANTIGRAVITY_CONVERSATION_ID, and the Front Desk itself resolves to false.
// The walk mirrors the one inside AttachWorktreeToTaskWithOptions, and additionally refuses a matched entry whose
// ConversationID is the Front Desk. It is read-only: it reads tasks.json, nested_conversations.json, and
// front_desk.json without taking any store lock.
func ResolveOwningTask(vaultDir, convOrTaskID string) (ActiveTask, bool) {
	cur := strings.TrimSpace(convOrTaskID)
	if cur == "" {
		cur = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	if cur == "" || IsFrontDeskConversation(vaultDir, cur) {
		return ActiveTask{}, false
	}
	tasks, err := ListTasks(vaultDir)
	if err != nil || len(tasks) == 0 {
		return ActiveTask{}, false
	}
	nestedMap, _ := loadNestedConversations(vaultDir)
	deskConv := ""
	if fd, ok := GetFrontDesk(vaultDir); ok {
		deskConv = strings.TrimSpace(fd.ConversationID)
	}
	idx := findOwningTaskIndex(tasks, nestedMap, deskConv, cur)
	if idx < 0 {
		return ActiveTask{}, false
	}
	return tasks[idx], true
}
