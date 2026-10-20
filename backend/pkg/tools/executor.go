package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"time"

	"pentagi/pkg/database"
	obs "pentagi/pkg/observability"
	"pentagi/pkg/observability/langfuse"
	"pentagi/pkg/schema"

	"github.com/sirupsen/logrus"
	"github.com/vxcontrol/langchaingo/documentloaders"
	"github.com/vxcontrol/langchaingo/llms"
	"github.com/vxcontrol/langchaingo/textsplitter"
	"github.com/vxcontrol/langchaingo/vectorstores/pgvector"
)

const DefaultResultSizeLimit = 16 * 1024 // 16 KB

// toolResultPersistTimeout bounds the writes that persist a tool result once
// the handler has already succeeded. They run on a context detached from the
// caller's cancellation so that an exhausted budget cannot discard a result
// the agent has already earned. The deadline starts when the write begins,
// because the tool call itself routinely runs for minutes.
const toolResultPersistTimeout = 10 * time.Second

const maxArgValueLength = 1024 // 1 KB limit for argument values

type dummyMessage struct {
	Message string `json:"message"`
}

// observationWrapper wraps different observation types with unified interface
type observationWrapper interface {
	ctx() context.Context
	end(result string, err error, durationSeconds float64)
}

// toolObservationWrapper wraps TOOL observation
type toolObservationWrapper struct {
	context     context.Context
	observation langfuse.Tool
}

func (w *toolObservationWrapper) ctx() context.Context {
	return w.context
}

func (w *toolObservationWrapper) end(result string, err error, durationSeconds float64) {
	opts := []langfuse.ToolOption{
		langfuse.WithToolOutput(result),
	}
	if err != nil {
		opts = append(opts,
			langfuse.WithToolStatus(err.Error()),
			langfuse.WithToolLevel(langfuse.ObservationLevelError),
		)
	} else {
		opts = append(opts,
			langfuse.WithToolStatus("success"),
		)
	}
	w.observation.End(opts...)
}

// agentObservationWrapper wraps AGENT observation
type agentObservationWrapper struct {
	context     context.Context
	observation langfuse.Agent
}

func (w *agentObservationWrapper) ctx() context.Context {
	return w.context
}

func (w *agentObservationWrapper) end(result string, err error, durationSeconds float64) {
	opts := []langfuse.AgentOption{
		langfuse.WithAgentOutput(result),
	}
	if err != nil {
		opts = append(opts,
			langfuse.WithAgentStatus(err.Error()),
			langfuse.WithAgentLevel(langfuse.ObservationLevelError),
		)
	} else {
		opts = append(opts,
			langfuse.WithAgentStatus("success"),
		)
	}
	w.observation.End(opts...)
}

// spanObservationWrapper wraps SPAN observation (used for barrier tools)
type spanObservationWrapper struct {
	context     context.Context
	observation langfuse.Span
}

func (w *spanObservationWrapper) ctx() context.Context {
	return w.context
}

func (w *spanObservationWrapper) end(result string, err error, durationSeconds float64) {
	opts := []langfuse.SpanOption{
		langfuse.WithSpanOutput(result),
	}
	if err != nil {
		opts = append(opts,
			langfuse.WithSpanStatus(err.Error()),
			langfuse.WithSpanLevel(langfuse.ObservationLevelError),
		)
	} else {
		opts = append(opts,
			langfuse.WithSpanStatus("success"),
		)
	}
	w.observation.End(opts...)
}

// noopObservationWrapper is a no-op wrapper for tools that create observations internally
type noopObservationWrapper struct {
	context context.Context
}

func (w *noopObservationWrapper) ctx() context.Context {
	return w.context
}

func (w *noopObservationWrapper) end(result string, err error, durationSeconds float64) {
}

type customExecutor struct {
	userID    int64
	flowID    int64
	taskID    *int64
	subtaskID *int64

	db    database.Querier
	mlp   MsgLogProvider
	tclp  ToolCallLogProvider
	store *pgvector.Store
	vslp  VectorStoreLogProvider

	definitions []llms.FunctionDefinition
	handlers    map[string]ExecutorHandler
	barriers    map[string]struct{}
	summarizer  SummarizeHandler
}

func (ce *customExecutor) Tools() []llms.Tool {
	tools := make([]llms.Tool, 0, len(ce.definitions))
	for idx := range ce.definitions {
		tools = append(tools, llms.Tool{
			Type:     "function",
			Function: &ce.definitions[idx],
		})
	}

	return tools
}

func (ce *customExecutor) createToolObservation(ctx context.Context, name string, args json.RawMessage) observationWrapper {
	ctx, observation := obs.Observer.NewObservation(ctx)
	metadata := langfuse.Metadata{
		"tool_name":     name,
		"tool_category": GetToolType(name).String(),
		"flow_id":       ce.flowID,
	}
	if ce.taskID != nil {
		metadata["task_id"] = *ce.taskID
	}
	if ce.subtaskID != nil {
		metadata["subtask_id"] = *ce.subtaskID
	}

	tool := observation.Tool(
		langfuse.WithToolName(name),
		langfuse.WithToolInput(args),
		langfuse.WithToolMetadata(metadata),
	)
	ctx, _ = tool.Observation(ctx)

	return &toolObservationWrapper{
		context:     ctx,
		observation: tool,
	}
}

func (ce *customExecutor) createAgentObservation(ctx context.Context, name string, args json.RawMessage) observationWrapper {
	ctx, observation := obs.Observer.NewObservation(ctx)
	metadata := langfuse.Metadata{
		"agent_name":    name,
		"tool_category": GetToolType(name).String(),
		"flow_id":       ce.flowID,
	}
	if ce.taskID != nil {
		metadata["task_id"] = *ce.taskID
	}
	if ce.subtaskID != nil {
		metadata["subtask_id"] = *ce.subtaskID
	}

	agent := observation.Agent(
		langfuse.WithAgentName(name),
		langfuse.WithAgentInput(args),
		langfuse.WithAgentMetadata(metadata),
	)
	ctx, _ = agent.Observation(ctx)

	return &agentObservationWrapper{
		context:     ctx,
		observation: agent,
	}
}

func (ce *customExecutor) createSpanObservation(ctx context.Context, name string, args json.RawMessage) observationWrapper {
	ctx, observation := obs.Observer.NewObservation(ctx)
	metadata := langfuse.Metadata{
		"barrier_name":  name,
		"tool_category": GetToolType(name).String(),
		"flow_id":       ce.flowID,
	}
	if ce.taskID != nil {
		metadata["task_id"] = *ce.taskID
	}
	if ce.subtaskID != nil {
		metadata["subtask_id"] = *ce.subtaskID
	}

	span := observation.Span(
		langfuse.WithSpanName(name),
		langfuse.WithSpanInput(args),
		langfuse.WithSpanMetadata(metadata),
	)
	ctx, _ = span.Observation(ctx)

	return &spanObservationWrapper{
		context:     ctx,
		observation: span,
	}
}

func (ce *customExecutor) Execute(
	ctx context.Context,
	streamID int64,
	id, name, obsName, thinking string,
	args json.RawMessage,
) (string, error) {
	startTime := time.Now()

	handler, ok := ce.handlers[name]
	if !ok {
		return fmt.Sprintf("function '%s' not found in available tools list", name), nil
	}

	var raw json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return fmt.Sprintf("failed to unmarshal '%s' tool call arguments: %v: fix it", name, err), nil
	}

	toolType := GetToolType(name)
	var obsWrapper observationWrapper

	// The arguments are persisted in several places and are readable through
	// the API, so only the masked copy leaves the executor. The handler below
	// still receives the original arguments.
	safeArgs := RedactToolArgs(args)

	switch toolType {
	case EnvironmentToolType, SearchNetworkToolType, StoreAgentResultToolType, StoreVectorDbToolType:
		obsWrapper = ce.createToolObservation(ctx, obsName, safeArgs)
	case AgentToolType:
		obsWrapper = ce.createAgentObservation(ctx, obsName, safeArgs)
	case BarrierToolType:
		obsWrapper = ce.createSpanObservation(ctx, obsName, safeArgs)
	case SearchVectorDbToolType:
		// Skip - handlers create RETRIEVER internally
		obsWrapper = &noopObservationWrapper{context: ctx}
	default:
		obsWrapper = &noopObservationWrapper{context: ctx}
	}

	ctx = obsWrapper.ctx()

	var err error
	msgID, msg := int64(0), ce.getMessage(safeArgs)
	if strings.Trim(msg, " \t\n\r") != "" {
		msgType := getMessageType(name)
		msgID, err = ce.mlp.PutMsg(ctx, msgType, ce.taskID, ce.subtaskID, streamID, thinking, msg)
		if err != nil {
			return "", err
		}
	}

	tcID, err := ce.tclp.PutLog(ctx, id, name, safeArgs, ce.taskID, ce.subtaskID)
	if err != nil {
		obsWrapper.end("", err, time.Since(startTime).Seconds())
		return "", fmt.Errorf("failed to create toolcall: %w", err)
	}

	wrapHandler := func(ctx context.Context, name string, args json.RawMessage) (string, database.MsglogResultFormat, error) {
		resultFormat := getMessageResultFormat(name)
		result, err := handler(ctx, name, args)

		// The summarizer is an LLM call and may take as long as it needs, so
		// it runs on a context detached from the caller's cancellation
		// without a deadline of its own.
		detachCtx := context.WithoutCancel(ctx)

		if err != nil {
			durationDelta := time.Since(startTime).Seconds()
			failureResult := fmt.Sprintf("failed to execute handler: %s", err.Error())
			logCtx, cancelLog := newPersistContext(ctx)
			defer cancelLog()
			_ = ce.tclp.UpdateLogFailed(logCtx, tcID, failureResult, durationDelta)
			return "", resultFormat, fmt.Errorf("failed to execute handler: %w", err)
		}

		result = database.SanitizeUTF8(result)
		allowSummarize := slices.Contains(allowedSummarizingToolsResult, name)
		if ce.summarizer != nil && allowSummarize && len(result) > DefaultResultSizeLimit {
			summarizePrompt, err := ce.getSummarizePrompt(name, string(safeArgs), result)
			if err != nil {
				return "", resultFormat, fmt.Errorf("failed to get summarize prompt: %w", err)
			}
			result, err = ce.summarizer(detachCtx, summarizePrompt)
			if err != nil {
				durationDelta := time.Since(startTime).Seconds()
				failureResult := fmt.Sprintf("failed to summarize result: %s", err.Error())
				logCtx, cancelLog := newPersistContext(ctx)
				defer cancelLog()
				_ = ce.tclp.UpdateLogFailed(logCtx, tcID, failureResult, durationDelta)
				return "", resultFormat, fmt.Errorf("failed to summarize result: %w", err)
			}
			resultFormat = database.MsglogResultFormatMarkdown
		} else if allowSummarize && len(result) > DefaultResultSizeLimit*2 {
			result = fmt.Sprintf("%s\n[0:%d bytes]\n... [truncated] ...\n[%d:%d bytes]\n%s",
				result[:DefaultResultSizeLimit],
				DefaultResultSizeLimit,
				len(result)-DefaultResultSizeLimit,
				len(result),
				result[len(result)-DefaultResultSizeLimit:],
			)
		}

		durationDelta := time.Since(startTime).Seconds()
		logCtx, cancelLog := newPersistContext(ctx)
		defer cancelLog()
		if err := ce.tclp.UpdateLogSuccess(logCtx, tcID, result, durationDelta); err != nil {
			// The toolcall row could not be updated, but the tool did produce
			// a result and the agent must still receive it.
			ce.logPersistFailure(logCtx, name, "failed to update toolcall result", err, nil)
		}

		return result, resultFormat, nil
	}

	if msg == "" { // no arg message to log and execute handler immediately
		result, _, err := wrapHandler(ctx, name, args)
		obsWrapper.end(result, err, time.Since(startTime).Seconds())
		return result, err
	}

	result, resultFormat, err := wrapHandler(ctx, name, args)
	if err != nil {
		obsWrapper.end(result, err, time.Since(startTime).Seconds())
		return "", err
	}

	// The handler already produced a result. Persisting it must never be able
	// to throw that result away: the caller's context may already be cancelled
	// (budget exhausted, task timeout), and the writes below used to run
	// against it, so a late cancellation discarded the recovered data before
	// the agent ever saw it. Each write gets its own context, created here
	// after the handler returned, so that the deadline covers the write and
	// not the tool call.
	storeCtx, cancelStore := newPersistContext(ctx)
	defer cancelStore()

	if err := ce.storeToolResult(storeCtx, name, result, safeArgs); err != nil {
		// Report the failure but keep the result: the caller turns a non-nil
		// error into an aborted agent chain, which would hide the data the
		// tool has already produced.
		ce.logPersistFailure(storeCtx, name, "failed to store tool result in long-term memory", err, nil)
	}

	if msgID != 0 {
		msgCtx, cancelMsg := newPersistContext(ctx)
		defer cancelMsg()
		if err := ce.mlp.UpdateMsgResult(msgCtx, msgID, streamID, result, resultFormat); err != nil {
			ce.logPersistFailure(msgCtx, name, "failed to update message result", err, logrus.Fields{
				"msg_id": msgID,
			})
		}
	}

	obsWrapper.end(result, nil, time.Since(startTime).Seconds())

	return result, nil
}

// newPersistContext returns the context for a write that persists a tool
// result after the handler has already succeeded. It is detached from the
// caller's cancellation so that an exhausted budget cannot discard a result
// the agent has already earned, and it carries its own deadline so that the
// write cannot hang the caller. The deadline must only be started when the
// write begins: the tool call itself routinely runs for minutes.
func newPersistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), toolResultPersistTimeout)
}

// logPersistFailure reports a persistence failure without aborting the agent
// chain. The tool itself succeeded, so the failure is a server-side problem
// with the audit trail rather than a failed tool call.
func (ce *customExecutor) logPersistFailure(
	ctx context.Context,
	name string,
	message string,
	err error,
	extra logrus.Fields,
) {
	fields := enrichLogrusFields(ce.flowID, ce.taskID, ce.subtaskID, logrus.Fields{
		"func_name": name,
	})
	for key, value := range extra {
		fields[key] = value
	}
	logrus.WithContext(ctx).WithError(err).WithFields(fields).Warn(message)
}

func (ce *customExecutor) IsBarrierFunction(name string) bool {
	_, ok := ce.barriers[name]
	return ok
}

func (ce *customExecutor) IsFunctionExists(name string) bool {
	_, ok := ce.handlers[name]
	return ok
}

func (ce *customExecutor) GetBarrierToolNames() []string {
	names := make([]string, 0, len(ce.barriers))
	for name := range ce.barriers {
		names = append(names, name)
	}

	return names
}

func (ce *customExecutor) GetBarrierTools() []FunctionInfo {
	tools := make([]FunctionInfo, 0, len(ce.barriers))
	for name := range ce.barriers {
		schema, err := ce.GetToolSchema(name)
		if err != nil {
			continue
		}
		schemaJSON, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		tools = append(tools, FunctionInfo{Name: name, Schema: string(schemaJSON)})
	}
	return tools
}

func (ce *customExecutor) GetToolSchema(name string) (*schema.Schema, error) {
	for _, def := range ce.definitions {
		if def.Name == name {
			return ce.converToJSONSchema(def.Parameters)
		}
	}

	if def, ok := registryDefinitions[name]; ok {
		return ce.converToJSONSchema(def.Parameters)
	}

	return nil, fmt.Errorf("tool %s not found", name)
}

func (ce *customExecutor) converToJSONSchema(params any) (*schema.Schema, error) {
	jsonSchema, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal parameters: %w", err)
	}

	var schema schema.Schema
	if err := json.Unmarshal(jsonSchema, &schema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal schema: %w", err)
	}

	return &schema, nil
}

func (ce *customExecutor) getSummarizePrompt(funcName, funcArgs, result string) (string, error) {
	templateText := `<instructions>
TASK: Summarize the execution result from '{{.FuncName}}' function call

DATA:
- <function> contains structured information about the function call
- <arguments> contains the parameters passed to the function
- <schema> contains the JSON schema of the function parameters
- <result> contains the raw output that NEEDS summarization

REQUIREMENTS:
1. Create a focused summary (max {{.MaxLength}} chars) that preserves critical information
2. Keep all actionable insights, technical details, and information relevant to the function's purpose
3. Preserve exact error messages, file paths, URLs, commands, and technical terminology
4. Structure information logically with appropriate formatting (headings, bullet points)
5. Begin with what the function accomplished or attempted

The summary must provide the same practical value as the original while being concise.
</instructions>

<function name="{{.FuncName}}">
<arguments>
{{.FormattedArgs}}
</arguments>
<schema>
{{.SchemaJSON}}
</schema>
</function>

<result>
{{.Result}}
</result>`

	var argsMap map[string]interface{}
	if err := json.Unmarshal([]byte(funcArgs), &argsMap); err != nil {
		return "", fmt.Errorf("failed to parse function arguments: %w", err)
	}

	var formattedArgs strings.Builder
	for key, value := range argsMap {
		strValue := fmt.Sprintf("%v", value)
		if len(strValue) > maxArgValueLength {
			strValue = strValue[:maxArgValueLength] + "... [truncated]"
		}
		formattedArgs.WriteString(fmt.Sprintf("%s: %s\n", key, strValue))
	}

	var schemaJSON string
	schemaObj, err := ce.GetToolSchema(funcName)
	if err == nil && schemaObj != nil {
		schemaBytes, err := json.MarshalIndent(schemaObj, "", "  ")
		if err == nil {
			schemaJSON = string(schemaBytes)
		}
	}

	templateContext := map[string]interface{}{
		"FuncName":      funcName,
		"FormattedArgs": formattedArgs.String(),
		"SchemaJSON":    schemaJSON,
		"Result":        result,
		"MaxLength":     DefaultResultSizeLimit / 2,
	}

	tmpl, err := template.New("summarize").Parse(templateText)
	if err != nil {
		return "", fmt.Errorf("error creating template: %v", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, templateContext); err != nil {
		return "", fmt.Errorf("error executing template: %v", err)
	}

	return buf.String(), nil
}

func (ce *customExecutor) getMessage(args json.RawMessage) string {
	var msg dummyMessage
	if err := json.Unmarshal(args, &msg); err != nil {
		return ""
	}

	return msg.Message
}

func (ce *customExecutor) storeToolResult(ctx context.Context, name, result string, args json.RawMessage) error {
	if ce.store == nil {
		return nil
	}

	if !slices.Contains(allowedStoringInMemoryTools, name) {
		return nil
	}

	var buffer strings.Builder
	buffer.WriteString(fmt.Sprintf("### Incoming arguments\n\n```json\n%s\n```\n\n", args))
	buffer.WriteString(fmt.Sprintf("#### Tool result\n\n%s\n\n", result))
	text := buffer.String()

	split := textsplitter.NewRecursiveCharacter(
		textsplitter.WithChunkSize(2000),
		textsplitter.WithChunkOverlap(100),
		textsplitter.WithCodeBlocks(true),
		textsplitter.WithHeadingHierarchy(true),
	)
	docs, err := documentloaders.NewText(strings.NewReader(text)).LoadAndSplit(ctx, split)
	if err != nil {
		return fmt.Errorf("failed to split tool result: %w", err)
	}

	for _, doc := range docs {
		if doc.Metadata == nil {
			doc.Metadata = map[string]any{}
		}
		doc.Metadata["user_id"] = ce.userID
		doc.Metadata["flow_id"] = ce.flowID
		if ce.taskID != nil {
			doc.Metadata["task_id"] = *ce.taskID
		}
		if ce.subtaskID != nil {
			doc.Metadata["subtask_id"] = *ce.subtaskID
		}
		doc.Metadata["tool_name"] = name
		if def, ok := registryDefinitions[name]; ok {
			doc.Metadata["tool_description"] = def.Description
		}
		doc.Metadata["doc_type"] = memoryVectorStoreDefaultType
		doc.Metadata["part_size"] = len(doc.PageContent)
		doc.Metadata["total_size"] = len(text)
	}

	if _, err := ce.store.AddDocuments(ctx, docs); err != nil {
		return fmt.Errorf("failed to store tool result: %w", err)
	}

	if agentCtx, ok := GetAgentContext(ctx); ok {
		data := map[string]any{
			"doc_type":  memoryVectorStoreDefaultType,
			"tool_name": name,
			"flow_id":   ce.flowID,
		}
		if ce.taskID != nil {
			data["task_id"] = *ce.taskID
		}
		if ce.subtaskID != nil {
			data["subtask_id"] = *ce.subtaskID
		}
		filtersData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal filters: %w", err)
		}
		query, err := ce.argsToMarkdown(args)
		if err != nil {
			return fmt.Errorf("failed to convert arguments to markdown: %w", err)
		}
		_, _ = ce.vslp.PutLog(
			ctx,
			agentCtx.ParentAgentType,
			agentCtx.CurrentAgentType,
			filtersData,
			query,
			database.VecstoreActionTypeStore,
			result,
			ce.taskID,
			ce.subtaskID,
		)
	}

	return nil
}

func (ce *customExecutor) argsToMarkdown(args json.RawMessage) (string, error) {
	var argsMap map[string]any
	if err := json.Unmarshal(args, &argsMap); err != nil {
		return "", fmt.Errorf("failed to unmarshal arguments: %w", err)
	}

	var buffer strings.Builder
	for key, value := range argsMap {
		if key == "message" {
			continue
		}
		buffer.WriteString(fmt.Sprintf("* %s: %v\n", key, value))
	}

	return buffer.String(), nil
}
