package loomruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

type memberProcessConfig struct {
	DSN, SearchPath, Directory, Report, Mode string
	Epoch                                    int64
}
type memberProcessReport struct {
	Phase, RunID, Error string
	Generation, Seq     int64
}

// Invoked by an actual child process, never by the ordinary package test run.
func TestMemberProcessHelper(t *testing.T) {
	raw := os.Getenv("WEAVE_MEMBER_PROCESS_TEST")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var config memberProcessConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	pgConfig, err := pgxpool.ParseConfig(config.DSN)
	if err != nil {
		t.Fatal(err)
	}
	pgConfig.ConnConfig.RuntimeParams["search_path"] = config.SearchPath
	pool, err := pgxpool.NewWithConfig(t.Context(), pgConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	h := memberPGFixture(t, pool, config.Directory)
	request := h.request
	request.ParentGeneration = config.Epoch
	request.ParentGuard = func(ctx context.Context, tx pgx.Tx) error {
		var epoch int64
		if err := tx.QueryRow(ctx, `SELECT epoch FROM member_test_parent FOR UPDATE`).Scan(&epoch); err != nil {
			return err
		}
		if epoch != config.Epoch {
			return ErrAttemptLeaseOwnerConflict
		}
		return nil
	}
	if config.Mode != "tool_wait" && config.Mode != "resume_tools" {
		opts := InstallFrozenMemberJournal(compiler.FrozenBuildOpts{})
		g := loom.NewGraph("workspace:member", "chat", loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
		g.SetHooks(loom.HookPoints{Before: opts.Hooks.BeforeStepHooks, After: opts.Hooks.AfterStepHooks})
		g.AddStep("chat", func(context.Context, loom.State) (loom.State, error) {
			file, err := os.OpenFile(filepath.Join(config.Directory, "graph-effects.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			_, err = fmt.Fprintln(file, "execute")
			closeErr := file.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
			return loom.State{"output": "done"}, nil
		}, loom.End())
		request.Graph = g
	}
	runner, _ := NewMemberRunner(h.records)
	write := func(report memberProcessReport) {
		t.Helper()
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.Report, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	reportFor := func(phase string) memberProcessReport {
		id := MemberRunID(request.WorkspaceID, request.ParentRunID, request.RunSnapshotID, request.NodeID, request.CallID)
		report := memberProcessReport{Phase: phase, RunID: id}
		if err := pool.QueryRow(t.Context(), `SELECT lease.attempt_generation,member.checkpoint_seq FROM weave_workflow_member_runs member
   JOIN weave_run_attempt_leases lease ON member.workspace_id=lease.workspace_id AND member.member_run_id=lease.run_id
   WHERE member.member_run_id=$1`, id).Scan(&report.Generation, &report.Seq); err != nil {
			t.Fatal(err)
		}
		return report
	}
	switch config.Mode {
	case "admit_wait", "admit_probe":
		member, _, err := runner.admit(t.Context(), request)
		if config.Mode == "admit_probe" {
			if !errors.Is(err, ErrMemberBusy) {
				t.Fatalf("duplicate admission=%v", err)
			}
			write(memberProcessReport{Phase: "busy"})
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(t.Context(), memberExecutionKey{}, member)
		if err := memberBeforeStep(ctx, "chat", member.input); err != nil {
			t.Fatal(err)
		}
		write(reportFor("ready"))
		command := make([]byte, 1)
		if _, err := io.ReadFull(os.Stdin, command); err != nil {
			t.Fatal(err)
		}
		if command[0] != 'w' {
			t.Fatal("invalid helper command")
		}
		raw, err := (&memberCheckpointStore{member: member}).Get(t.Context(), "checkpoint:"+request.Graph.Name, member.runID)
		if err != nil {
			t.Fatal(err)
		}
		var cp memberCheckpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			t.Fatal(err)
		}
		cp.Seq++
		cp.State["__seq"] = cp.Seq
		cp.State["output"] = "stale"
		raw, err = json.Marshal(cp)
		if err != nil {
			t.Fatal(err)
		}
		if err := (&memberCheckpointStore{member: member}).Put(t.Context(), "checkpoint:"+request.Graph.Name, member.runID, raw); !errors.Is(err, ErrAttemptLeaseOwnerConflict) {
			t.Fatalf("old process write=%v", err)
		}
		write(reportFor("fenced"))
		return
	case "tool_wait":
		h.records.afterToolReceipt = func() { write(reportFor("ready")); command := make([]byte, 1); _, _ = io.ReadFull(os.Stdin, command) }
	case "resume_graph", "resume_tools":
	default:
		t.Fatal("invalid subprocess mode")
	}
	result, err := runner.Run(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != loom.StopCompleted {
		t.Fatalf("unexpected result=%+v", result)
	}
	write(reportFor("complete"))
}

type runningMemberProcess struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *os.File
	report  string
}

func startMemberProcess(t *testing.T, h *memberPGHarness, directory, mode, name string, epoch int64) *runningMemberProcess {
	t.Helper()
	config := memberProcessConfig{DSN: h.pool.Config().ConnString(), SearchPath: h.pool.Config().ConnConfig.RuntimeParams["search_path"], Directory: directory, Report: filepath.Join(directory, name+".json"), Mode: mode, Epoch: epoch}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMemberProcessHelper$", "-test.v")
	command.Env = append(os.Environ(), "WEAVE_MEMBER_PROCESS_TEST="+string(raw))
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(directory, name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &runningMemberProcess{command: command, input: input, output: output, report: config.Report}
	t.Cleanup(func() {
		_ = input.Close()
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = output.Close()
	})
	return process
}
func waitMemberProcessReport(t *testing.T, process *runningMemberProcess, phase string) memberProcessReport {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(process.report)
		var report memberProcessReport
		if err == nil && json.Unmarshal(raw, &report) == nil && report.Phase == phase {
			return report
		}
		time.Sleep(10 * time.Millisecond)
	}
	log, _ := os.ReadFile(process.output.Name())
	t.Fatalf("subprocess did not reach %s: %s", phase, log)
	return memberProcessReport{}
}
func finishMemberProcess(t *testing.T, p *runningMemberProcess) {
	t.Helper()
	if err := p.command.Wait(); err != nil {
		log, _ := os.ReadFile(p.output.Name())
		t.Fatalf("member subprocess: %v\n%s", err, log)
	}
}

func TestMemberSeparateProcessesFenceOldWriterAndReuseCompletionRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	directory := t.TempDir()
	old := startMemberProcess(t, h, directory, "admit_wait", "old", 1)
	ready := waitMemberProcessReport(t, old, "ready")
	duplicate := startMemberProcess(t, h, directory, "admit_probe", "duplicate", 1)
	finishMemberProcess(t, duplicate)
	if _, err := h.pool.Exec(t.Context(), `UPDATE member_test_parent SET epoch=2`); err != nil {
		t.Fatal(err)
	}
	next := startMemberProcess(t, h, directory, "resume_graph", "new", 2)
	complete := waitMemberProcessReport(t, next, "complete")
	finishMemberProcess(t, next)
	if ready.RunID != complete.RunID || ready.Generation != 1 || complete.Generation != 2 || complete.Seq <= ready.Seq {
		t.Fatalf("identity/continuity: old=%+v next=%+v", ready, complete)
	}
	if _, err := old.input.Write([]byte("w")); err != nil {
		t.Fatal(err)
	}
	fenced := waitMemberProcessReport(t, old, "fenced")
	finishMemberProcess(t, old)
	if fenced.Seq != complete.Seq {
		t.Fatalf("old process changed latest: before=%d after=%d", complete.Seq, fenced.Seq)
	}
	receiver := startMemberProcess(t, h, directory, "resume_graph", "receiver", 2)
	finishMemberProcess(t, receiver)
	effects, err := os.ReadFile(filepath.Join(directory, "graph-effects.txt"))
	if err != nil || string(effects) != "execute\n" {
		t.Fatalf("completed member reexecuted: %q %v", effects, err)
	}
}

func TestMemberKilledProcessResumesConfirmedToolReceiptsRealPG(t *testing.T) {
	h := newMemberPGHarness(t)
	directory := t.TempDir()
	old := startMemberProcess(t, h, directory, "tool_wait", "killed", 1)
	ready := waitMemberProcessReport(t, old, "ready")
	if err := old.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := old.command.Wait(); err == nil {
		t.Fatal("expected killed process")
	}
	if _, err := h.pool.Exec(t.Context(), `UPDATE member_test_parent SET epoch=2`); err != nil {
		t.Fatal(err)
	}
	next := startMemberProcess(t, h, directory, "resume_tools", "restarted", 2)
	complete := waitMemberProcessReport(t, next, "complete")
	finishMemberProcess(t, next)
	if complete.RunID != ready.RunID || complete.Generation != 2 {
		t.Fatalf("new logical member allocated: before=%+v after=%+v", ready, complete)
	}
	effects, err := os.ReadFile(filepath.Join(directory, "effects.txt"))
	if err != nil || string(effects) != "call-a\ncall-b\n" {
		t.Fatalf("repeated effect: %q %v", effects, err)
	}
}
