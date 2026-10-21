// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Xustalis/OpenPanda/internal/core"
	"github.com/Xustalis/OpenPanda/internal/sessions"
)

// The readable text of a result is what the report is built from. It used to
// read only the "result" key, so an adapter that reports its answer on stdout
// produced an empty digest and the summary fell through to the risk level —
// the thread showed "[done] low" next to a completed task.
func TestExtractResultTextFindsTheAnswerWhereverItLives(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"structured result", `{"result":"done via result"}`, "done via result"},
		{"answer key", `{"answer":"done via answer"}`, "done via answer"},
		{"agent stdout", `{"ok":true,"stdout":"任务完成。\n\n编译无错误"}`, "任务完成。\n\n编译无错误"},
		{"failure stderr", `{"ok":false,"stderr":"gcc: not found"}`, "gcc: not found"},
		{"failure reason", `{"failed":"lease expired"}`, "lease expired"},
		{"result wins over stdout", `{"result":"best","stdout":"noise"}`, "best"},
		{"blank values are skipped", `{"result":"   ","stdout":"real text"}`, "real text"},
		{"nothing readable", `{"ok":true}`, ""},
		{"not an object", `"plain"`, ""},
		{"empty payload", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractResultText(c.json); got != c.want {
				t.Fatalf("extractResultText(%q) = %q, want %q", c.json, got, c.want)
			}
		})
	}
}

// Extraction itself never truncates: it hands the whole answer to the report,
// which is the layer that decides how much of it the thread shows.
func TestExtractResultTextKeepsTheWholeAnswer(t *testing.T) {
	long := strings.Repeat("汉", 2000)
	got := extractResultText(`{"result":"` + long + `"}`)
	if got != long {
		t.Fatalf("text was cut: %d chars of %d", len(got), len(long))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("text is not valid UTF-8: %q", got)
	}
	if short := extractResultText(`{"result":"短"}`); short != "短" {
		t.Fatalf("short text was mangled: %q", short)
	}
}

// The thread shows the closing paragraph of an employee's reply, not the
// whole reply. Sections above it are the evidence for that conclusion.
func TestConclusionOfTakesTheClosingParagraph(t *testing.T) {
	reply := "完成。新建 `pelican-bike.html`，纯内联 SVG。\n\n## 变更\n\n- 新建文件\n- 无外部引用\n\n## 验证\n\n三帧截图显示动画持续推进，无图形错位。"
	if got := conclusionOf(reply); got != "三帧截图显示动画持续推进，无图形错位。" {
		t.Fatalf("conclusion = %q", got)
	}

	// One paragraph is already the conclusion — it comes back untouched.
	if got := conclusionOf("已完成，服务在 8080 端口运行。"); got != "已完成，服务在 8080 端口运行。" {
		t.Fatalf("single-paragraph conclusion = %q", got)
	}
}

// What cannot be a conclusion is skipped, not printed: a trailing question is
// a prompt for the next turn, and a code fence or table is evidence.
func TestConclusionOfSkipsWhatIsNotAVerdict(t *testing.T) {
	asked := "已生成网页并验证。\n\n需要我继续调整配色吗？"
	if got := conclusionOf(asked); got != "已生成网页并验证。" {
		t.Fatalf("trailing question was taken as the conclusion: %q", got)
	}

	fenced := "构建通过。\n\n```\n$ go build ./...\n```"
	if got := conclusionOf(fenced); got != "构建通过。" {
		t.Fatalf("code fence was taken as the conclusion: %q", got)
	}

	heading := "迁移已完成。\n\n## 下一步"
	if got := conclusionOf(heading); got != "迁移已完成。" {
		t.Fatalf("bare heading was taken as the conclusion: %q", got)
	}

	// Nothing qualifies: the tail is still better than nothing at all.
	if got := conclusionOf("？"); got != "？" {
		t.Fatalf("empty-ish reply = %q", got)
	}
}

// A conclusion is a paragraph, but a pathological one-paragraph answer is
// still bounded — at a sentence break, on runes, so Chinese is not cut mid
// character and the reader does not get half a word.
func TestConclusionOfClipsAtASentenceBreak(t *testing.T) {
	sentence := "这一段是用来占位的完整句子。"
	reply := strings.Repeat(sentence, 200) // ~2800 runes, one paragraph
	got := conclusionOf(reply)
	if utf8.RuneCountInString(got) > maxConclusionRunes+1 {
		t.Fatalf("conclusion ran to %d runes, cap is %d", utf8.RuneCountInString(got), maxConclusionRunes)
	}
	if !strings.HasSuffix(got, "。…") {
		t.Fatalf("conclusion was not cut at a sentence break: %q", got[len(got)-12:])
	}
	if !utf8.ValidString(got) {
		t.Fatal("conclusion is not valid UTF-8")
	}
}

// The report is two parts: the orchestration line, then the conclusion.
func TestTaskSummaryLeadsWithWhoRanIt(t *testing.T) {
	done := core.Task{
		State:      core.StateDone,
		Risk:       "low",
		Title:      "2D SVG 鹈鹕骑车网页",
		ResultJSON: `{"ok":true,"stdout":"完成。新建 pelican-bike.html。\n\n动画为纯 SMIL，无 JS。"}`,
	}
	want := "[done] @claude_code · 2D SVG 鹈鹕骑车网页\n动画为纯 SMIL，无 JS。"
	if got := summaryText(done, "", "claude_code"); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	// A run with no attribution still names the work, just without a person.
	if got := summaryText(done, "", ""); !strings.HasPrefix(got, "[done] 2D SVG 鹈鹕骑车网页\n") {
		t.Fatalf("unattributed summary = %q", got)
	}
}

// A failed row with no payload still owes the reader a reason — the one its
// last result event recorded. Without it the thread says "[failed] low".
func TestTaskSummaryCarriesTheFailureReason(t *testing.T) {
	failed := core.Task{State: core.StateFailed, Risk: "low", Title: "总结仓库"}
	want := "[failed] @claude_code · 总结仓库\nlease expired"
	if got := summaryText(failed, "lease expired", "claude_code"); got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}

	// Nothing anywhere: the orchestration line alone is still honest.
	if got := summaryText(core.Task{State: core.StateFailed, Title: "总结仓库"}, "", ""); got != "[failed] 总结仓库" {
		t.Fatalf("bare summary = %q", got)
	}

	// A finished task never shows a stale reason from an earlier attempt.
	if got := summaryText(core.Task{State: core.StateDone, Title: "写个网页"}, "reason ignored", ""); got != "[done] 写个网页" {
		t.Fatalf("done summary = %q", got)
	}
}

// The sweep is what actually writes the turn: the employee named in the
// orchestration line comes from the timeline (a local run never puts it in the
// payload), and only the closing paragraph of the reply reaches the thread.
func TestFinalizerWritesTheOrchestrationLineAndTheConclusion(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	sessStore := sessions.NewStore(t.TempDir())
	s, err := sessStore.Create("thread", "")
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{store: store, sessions: sessStore}

	task, err := store.Create(ctx, "", "proj", "2D SVG 鹈鹕骑车网页", "node", []string{"node"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetQueueMeta(ctx, task.TaskID, core.PriorityNormal, s.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return store.Queue(ctx, task.TaskID, "node") },
		func() error { return store.Dispatch(ctx, task.TaskID, "node", "node") },
		func() error { return store.Accept(ctx, task.TaskID, "node") },
		func() error {
			return store.RecordEvent(ctx, task.TaskID, "exec_agent_start", map[string]any{
				"agent": "claude_code", "model": "claude_code",
			})
		},
		func() error {
			return store.Complete(ctx, task.TaskID, "node", map[string]any{
				"ok":     true,
				"stdout": "完成。新建 `pelican-bike.html`。\n\n## 验证\n\n三帧截图显示动画持续推进，无图形错位。",
			})
		},
	} {
		if err := step(); err != nil {
			t.Fatalf("drive to done: %v", err)
		}
	}

	h.finalizeSessionSweep(ctx)

	got, err := sessStore.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Turns) != 1 {
		t.Fatalf("turns = %d, want the one report turn", len(got.Turns))
	}
	want := "[done] @claude_code · 2D SVG 鹈鹕骑车网页\n三帧截图显示动画持续推进，无图形错位。"
	if got.Turns[0].Text != want {
		t.Fatalf("report = %q, want %q", got.Turns[0].Text, want)
	}

	// A second sweep must not report the same task twice.
	h.finalizeSessionSweep(ctx)
	again, err := sessStore.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Turns) != 1 {
		t.Fatalf("turns after a second sweep = %d, want 1", len(again.Turns))
	}
}
