package cellnparent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A continued conversation: when an enduring parent's live context is lost,
// or an operator asks for a restart, a new run takes over on any node with
// capacity, seeded with the exchanges recorded so far. The seed is text the
// model already answered, never instructions; it travels in the new run's
// spec so the previous run can be gone by the time the parent starts.

// ResumeMessage is the first turn of a continued run: it makes the new
// parent show its memory rather than silently pretending nothing happened.
const ResumeMessage = "This conversation continues on a new node. In one short sentence, say what we were discussing so far; do not use tools."

// ContinuedFromAnnotation marks a run created to continue another.
const ContinuedFromAnnotation = "sympozium.ai/continued-from"

// The parent carries history and the next message inside one bounded turn
// (2048 bytes); a seed must leave room for that message. These mirror the
// host's own bounds so a plan the controller builds is never refused there.
const (
	maxSeedExchanges = 16
	maxSeedBytes     = 2048 - 512
)

// Transcript gathers the committed exchanges of a run, oldest first: the
// seed it started with, its initial turn, then every succeeded follow-up
// turn. Failed turns are not part of the conversation's memory.
func Transcript(ctx context.Context, reader client.Reader, run *api.AgentRun) ([]api.ConversationExchange, error) {
	var exchanges []api.ConversationExchange
	if run.Spec.Conversation != nil {
		exchanges = append(exchanges, run.Spec.Conversation.Seed...)
	}
	if run.Status.CellnParent != nil && run.Status.CellnParent.InitialTurn != nil && run.Status.CellnParent.InitialTurn.Result != nil && run.Status.CellnParent.InitialTurn.Result.Succeeded && run.Spec.Task != nil && run.Spec.Task.IsString() {
		exchanges = append(exchanges, api.ConversationExchange{User: run.Spec.Task.GetPrompt(), Assistant: run.Status.CellnParent.InitialTurn.Result.Answer})
	}
	var list api.AgentRunTurnList
	if err := reader.List(ctx, &list, client.InNamespace(run.Namespace)); err != nil {
		return nil, err
	}
	turns := make([]api.AgentRunTurn, 0, len(list.Items))
	for _, turn := range list.Items {
		if turn.Spec.RunName == run.Name && turn.Spec.RunUID == string(run.UID) && turn.Status.Execution != nil && turn.Status.Execution.Result != nil && turn.Status.Execution.Result.Succeeded {
			turns = append(turns, turn)
		}
	}
	sort.Slice(turns, func(i, j int) bool {
		if turns[i].CreationTimestamp.Equal(&turns[j].CreationTimestamp) {
			return turns[i].Name < turns[j].Name
		}
		return turns[i].CreationTimestamp.Before(&turns[j].CreationTimestamp)
	})
	for _, turn := range turns {
		exchanges = append(exchanges, api.ConversationExchange{User: turn.Spec.Message, Assistant: turn.Status.Execution.Result.Answer})
	}
	return TrimSeed(exchanges), nil
}

// TrimSeed keeps the newest exchanges that fit the host's seed bounds. A
// conversation longer than the bound keeps its recent memory, not its start.
func TrimSeed(exchanges []api.ConversationExchange) []api.ConversationExchange {
	kept := make([]api.ConversationExchange, 0, len(exchanges))
	for _, exchange := range exchanges {
		if strings.TrimSpace(exchange.User) == "" || strings.ContainsRune(exchange.User, 0) || strings.ContainsRune(exchange.Assistant, 0) {
			continue
		}
		kept = append(kept, exchange)
	}
	for len(kept) > 0 && !SeedFits(kept) {
		kept = kept[1:]
	}
	return kept
}

// SeedFits reports whether a seed is within the host's bounds.
func SeedFits(exchanges []api.ConversationExchange) bool {
	if len(exchanges) > maxSeedExchanges {
		return false
	}
	raw, err := json.Marshal(map[string]any{"history": exchanges, "message": ""})
	return err == nil && len(raw) <= maxSeedBytes
}

// Continuation builds the run that carries a conversation on from previous:
// the same Agent, model, selection and limits, the resume message as its
// initial turn, and the transcript as its seed. It is not created here.
func Continuation(previous *api.AgentRun, seed []api.ConversationExchange) (*api.AgentRun, error) {
	if previous.Spec.ExecutionLifecycle != "enduring" || previous.Spec.Enduring == nil {
		return nil, fmt.Errorf("only an enduring run can be continued")
	}
	depth := int32(1)
	continuation := "automatic"
	if previous.Spec.Conversation != nil {
		depth = previous.Spec.Conversation.Depth + 1
		if previous.Spec.Conversation.Continuation != "" {
			continuation = previous.Spec.Conversation.Continuation
		}
	}
	if depth > api.MaxContinuationDepth {
		return nil, fmt.Errorf("conversation continued %d times; not continuing again", depth-1)
	}
	if !SeedFits(seed) {
		return nil, fmt.Errorf("seed exceeds the parent's context bound")
	}
	spec := previous.Spec.DeepCopy()
	spec.Task = api.NewStringTask(ResumeMessage)
	spec.Conversation = &api.ConversationSpec{Continuation: continuation, ContinuesFrom: previous.Name, Depth: depth, Seed: seed}
	labels := map[string]string{}
	for key, value := range previous.Labels {
		labels[key] = value
	}
	annotations := map[string]string{ContinuedFromAnnotation: previous.Name}
	return &api.AgentRun{
		ObjectMeta: metav1.ObjectMeta{GenerateName: previous.Spec.AgentRef + "-", Namespace: previous.Namespace, Labels: labels, Annotations: annotations},
		Spec:       *spec,
	}, nil
}
