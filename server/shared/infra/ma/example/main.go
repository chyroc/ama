// End-to-end example: create an Agent + Environment + Session, send one message and get the reply.
//
//	export ARK_API_KEY=...
//	export ARK_MODEL_ID=doubao-seed-2-1-pro-260628
//	go run ./shared/infra/ma/example
//
// This file is compilable usage documentation; the example really creates resources and cleans
// them up at the end.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

func main() {
	apiKey := os.Getenv("ARK_API_KEY")
	if apiKey == "" {
		log.Fatal("请先设置 ARK_API_KEY")
	}
	modelID := os.Getenv("ARK_MODEL_ID")
	if modelID == "" {
		modelID = "doubao-seed-2-1-pro-260628"
	}

	// Create an instance from an API Key; every later operation hangs off it.
	ins, err := ma.New(apiKey)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1. Agent: the agent configuration, with the platform's built-in toolset enabled.
	ag, err := ins.Agents().Create(ctx, ma.AgentSpec{
		Name:   fmt.Sprintf("ama-example-%d", time.Now().UnixNano()),
		Model:  modelID,
		System: "你是一个乐于助人的助手，回答尽量简短。",
		Tools:  []string{ma.ToolsetDefault},
	})
	if err != nil {
		log.Fatalf("创建 Agent 失败: %v", err)
	}
	defer cleanup("Agent", func() error {
		_, err := ins.Agents().Delete(context.Background(), ag.ID)
		return err
	})()
	fmt.Printf("Agent:       %s\n", ag.ID)

	// 2. Environment: a hosted sandbox with outbound traffic allowed.
	env, err := ins.Environments().Create(ctx, ma.EnvironmentSpec{
		Name: fmt.Sprintf("ama-example-env-%d", time.Now().UnixNano()),
	})
	if err != nil {
		log.Fatalf("创建 Environment 失败: %v", err)
	}
	defer cleanup("Environment", func() error {
		_, err := ins.Environments().Delete(context.Background(), env.ID)
		return err
	})()
	fmt.Printf("Environment: %s\n", env.ID)

	// 3. Session: bind the Agent to the environment.
	sess, err := ins.Sessions().Create(ctx, ma.SessionSpec{
		AgentID:       ag.ID,
		EnvironmentID: env.ID,
		Title:         "ma 包示例",
	})
	if err != nil {
		log.Fatalf("创建 Session 失败: %v", err)
	}
	defer cleanup("Session", func() error {
		_, err := ins.Sessions().Delete(context.Background(), sess.ID)
		return err
	})()
	fmt.Printf("Session:     %s\n\n", sess.ID)

	// 4. Send the message and wait for the Agent to finish. Run already handles the "open the
	//    stream before sending" ordering and the detection of terminal events.
	res, err := ins.Sessions().Run(ctx, sess.ID, "世界最高的山是哪座？一句话回答。")
	if err != nil {
		// Ark's structured error carries request_id, which helps when reporting a problem.
		if apiErr, ok := ma.AsAPIError(err); ok {
			log.Fatalf("执行失败 code=%s request_id=%s: %v",
				apiErr.Code, apiErr.RequestId, err)
		}
		log.Fatalf("执行失败: %v", err)
	}

	fmt.Printf("事件轨迹:    %v\n", res.Events)
	fmt.Printf("结束状态:    %s (正常空闲=%t)\n", res.TerminalType, res.Idle())
	fmt.Printf("Agent 回复:  %s\n", res.Text)
}

// cleanup wraps the cleanup and error reporting inside defer, so the example is not buried in
// error handling.
func cleanup(what string, fn func() error) func() {
	return func() {
		if err := fn(); err != nil {
			log.Printf("清理 %s 失败: %v", what, err)
		}
	}
}
