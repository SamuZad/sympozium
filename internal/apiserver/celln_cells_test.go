package apiserver

import (
	"encoding/json"
	"testing"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestJoinCellnCellsAttributesTurnsToRuns(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	child := "blake3:c8637381b4957825e2794a01e3fee6c6cb23c6be229dc6f1ff8322069b0ff61c"
	report := map[string]any{
		"apiVersion": "sympozium.ai/celln-node-cells-v1", "node": "framework", "reportedMs": now.UnixMilli() - 5000,
		"cells": []map[string]any{
			{"id": "2e1e3481f3b7", "description": "blake3:c8637381b4957825e2794a0…", "status": "running", "backend": "kvm", "started_ms": 1, "tools": []string{"/worker"}},
			{"id": "0000000000aa", "description": "blake3:ffff", "status": "dissolved", "backend": "kvm", "started_ms": 1, "finished_ms": 3, "duration_ms": 2},
		},
		"parents": []map[string]any{{"incarnation": "blake3:parent", "updatedMs": 1, "turns": []map[string]any{{"turnId": "initial", "stage": "reserved", "child": child}}}},
	}
	raw, _ := json.Marshal(report)
	stale := map[string]any{"apiVersion": "sympozium.ai/celln-node-cells-v1", "node": "old", "reportedMs": now.Add(-5 * time.Minute).UnixMilli()}
	staleRaw, _ := json.Marshal(stale)
	runs := []api.AgentRun{{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes-abc", Namespace: "default"},
		Spec:       api.AgentRunSpec{AgentRef: "hermes"},
		Status:     api.AgentRunStatus{Phase: api.AgentRunPhaseRunning, CellnParent: &api.CellnParentStatus{Binding: api.CellnParentBinding{Incarnation: "blake3:parent"}}},
	}}

	nodes := joinCellnCells(map[string]string{"framework": string(raw), "old": string(staleRaw), "broken": "{"}, runs, now)
	if len(nodes) != 3 || nodes[0].Node != "broken" || nodes[0].Error == "" || nodes[1].Node != "framework" || nodes[2].Node != "old" || !nodes[2].Stale {
		t.Fatalf("nodes not decoded, sorted and flagged: %+v", nodes)
	}
	fw := nodes[1]
	if fw.Stale || len(fw.Cells) != 2 || len(fw.Parents) != 1 {
		t.Fatalf("framework report: %+v", fw)
	}
	cell := fw.Cells[0]
	if cell.Run == nil || cell.Run.Name != "hermes-abc" || cell.Run.Agent != "hermes" || !cell.Run.Live || cell.Parent != "blake3:parent" || cell.Turn != "initial" {
		t.Fatalf("turn cell not attributed to its run: %+v %+v", cell, cell.Run)
	}
	if fw.Cells[1].Run != nil || fw.Parents[0].Run == nil || fw.Parents[0].Run.Phase != "Running" {
		t.Fatalf("unrelated cell attributed or parent not joined: %+v %+v", fw.Cells[1], fw.Parents[0])
	}
}
