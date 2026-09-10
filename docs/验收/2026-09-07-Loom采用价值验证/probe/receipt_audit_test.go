package main

import (
 "context"
 "os"
 "path/filepath"
 "testing"

 "github.com/jinyitao123/loom/contract"
)

// This safety regression intentionally fails against the archived probe.
// It tests shared B/C receipt infrastructure, not Loom's internal store.
func TestReceiptRejectsConflictingOperationIdentity(t *testing.T) {
 for _, candidate := range []string{"B", "C"} {
  for _, conflict := range []string{"tool", "arguments"} {
   t.Run(candidate+"/"+conflict,func(t *testing.T) {
    dir:=t.TempDir()
    for _, rel:=range []string{"model/tests/test_physics.py","task.md"} {
     path:=filepath.Join(dir,"workspace",rel)
     if err:=os.MkdirAll(filepath.Dir(path),0700);err!=nil {t.Fatal(err)}
     if err:=os.WriteFile(path,[]byte(rel),0600);err!=nil {t.Fatal(err)}
    }
    host:=toolHost{cfg:config{RunDir:dir,Candidate:candidate,Task:"t2",Scenario:"s0"}}
    first:=contract.ToolCall{ID:"operation-0001",Name:"read_file",Args:`{"path":"task.md"}`}
    if result,err:=host.Dispatch(context.Background(),first);err!=nil||result.IsError {t.Fatalf("setup failed: %v %v",result,err)}
    second:=first
    if conflict=="tool" {second.Name="different_tool"} else {second.Args=`{"path":"model/tests/test_physics.py"}`}
    result,err:=host.Dispatch(context.Background(),second)
    if err==nil && !result.IsError {t.Fatalf("identity conflict accepted: requested %s %s; returned tool=%s content=%s",second.Name,second.Args,result.ToolName,result.Content)}
   })
  }
 }
}
