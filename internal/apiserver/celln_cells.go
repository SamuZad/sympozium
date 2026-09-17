package apiserver

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellninstall"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// Each fleet node reports what `celln ps -a` shows for its authority root,
// and its recent parents, to one ConfigMap (charts/sympozium/files/celln/
// fleet-cells.py). This endpoint reads it and joins cells and parents to the
// AgentRuns they belong to.

// cellnNodeStaleAfter marks a node whose last report is older than this.
const cellnNodeStaleAfter = 90 * time.Second

// CellnCell is one cell as `celln ps -a` reports it.
type CellnCell struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Backend     string   `json:"backend"`
	StartedMs   int64    `json:"started_ms"`
	FinishedMs  *int64   `json:"finished_ms"`
	DurationMs  *int64   `json:"duration_ms"`
	Error       *string  `json:"error"`
	Tools       []string `json:"tools"`
	// Run is the AgentRun whose parent ran this cell as a turn, when known.
	Run *CellnRunRef `json:"run,omitempty"`
	// Parent is the parent incarnation the cell ran a turn for.
	Parent string `json:"parent,omitempty"`
	Turn   string `json:"turn,omitempty"`
}

// CellnParentTurn is one turn recorded in a parent's journal.
type CellnParentTurn struct {
	TurnID    string `json:"turnId"`
	Stage     string `json:"stage"`
	Child     string `json:"child,omitempty"`
	Succeeded *bool  `json:"succeeded,omitempty"`
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
}

// CellnNodeParent is one parent a node's journal records.
type CellnNodeParent struct {
	Incarnation string            `json:"incarnation"`
	UpdatedMs   int64             `json:"updatedMs"`
	Turns       []CellnParentTurn `json:"turns"`
	Run         *CellnRunRef      `json:"run,omitempty"`
}

// CellnRunRef names an AgentRun and its state.
type CellnRunRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	Phase     string `json:"phase"`
	// Live is true while the run is not finished, i.e. its parent should
	// still hold a context on the node.
	Live bool `json:"live"`
}

// CellnNodeCells is one node's report.
type CellnNodeCells struct {
	Node       string            `json:"node"`
	ReportedMs int64             `json:"reportedMs"`
	Stale      bool              `json:"stale"`
	Error      string            `json:"error,omitempty"`
	Cells      []CellnCell       `json:"cells"`
	Parents    []CellnNodeParent `json:"parents"`
}

type cellnNodeReport struct {
	APIVersion string            `json:"apiVersion"`
	Node       string            `json:"node"`
	ReportedMs int64             `json:"reportedMs"`
	Cells      []CellnCell       `json:"cells"`
	Parents    []CellnNodeParent `json:"parents"`
}

func (s *Server) listCellnFleetCells(w http.ResponseWriter, r *http.Request) {
	var cm corev1.ConfigMap
	err := s.client.Get(r.Context(), types.NamespacedName{Namespace: "celln-system", Name: cellninstall.FleetCellsConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		writeJSON(w, []CellnNodeCells{})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var runs api.AgentRunList
	if err := s.client.List(r.Context(), &runs); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, joinCellnCells(cm.Data, runs.Items, time.Now()))
}

// joinCellnCells decodes every node's report and attributes parents and the
// cells their turns ran in to AgentRuns by parent incarnation and child hash.
func joinCellnCells(data map[string]string, runs []api.AgentRun, now time.Time) []CellnNodeCells {
	type turnRef struct {
		run          *CellnRunRef
		parent, turn string
	}
	byIncarnation := map[string]*CellnRunRef{}
	for i := range runs {
		run := &runs[i]
		parent := run.Status.CellnParent
		if parent == nil || parent.Binding.Incarnation == "" {
			continue
		}
		phase := string(run.Status.Phase)
		byIncarnation[parent.Binding.Incarnation] = &CellnRunRef{
			Namespace: run.Namespace, Name: run.Name, Agent: run.Spec.AgentRef, Phase: phase,
			Live: run.DeletionTimestamp == nil && phase != "Succeeded" && phase != "Failed",
		}
	}
	out := make([]CellnNodeCells, 0, len(data))
	for node, raw := range data {
		entry := CellnNodeCells{Node: node, Cells: []CellnCell{}, Parents: []CellnNodeParent{}}
		var report cellnNodeReport
		if err := json.Unmarshal([]byte(raw), &report); err != nil || report.APIVersion != "sympozium.ai/celln-node-cells-v1" {
			entry.Error = "unreadable report"
			entry.Stale = true
			out = append(out, entry)
			continue
		}
		entry.ReportedMs = report.ReportedMs
		entry.Stale = now.Sub(time.UnixMilli(report.ReportedMs)) > cellnNodeStaleAfter
		// ps truncates a cell's description (the child hash) with an
		// ellipsis; match turns on that prefix.
		children := map[string]turnRef{}
		for _, p := range report.Parents {
			p.Run = byIncarnation[p.Incarnation]
			if p.Turns == nil {
				p.Turns = []CellnParentTurn{}
			}
			for _, t := range p.Turns {
				if t.Child != "" {
					children[t.Child] = turnRef{run: p.Run, parent: p.Incarnation, turn: t.TurnID}
				}
			}
			entry.Parents = append(entry.Parents, p)
		}
		for _, c := range report.Cells {
			prefix := strings.TrimSuffix(c.Description, "…")
			if len(prefix) > len("blake3:") {
				for child, ref := range children {
					if strings.HasPrefix(child, prefix) {
						c.Run, c.Parent, c.Turn = ref.run, ref.parent, ref.turn
						break
					}
				}
			}
			if c.Tools == nil {
				c.Tools = []string{}
			}
			entry.Cells = append(entry.Cells, c)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}
