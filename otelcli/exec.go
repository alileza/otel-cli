package otelcli

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/equinix-labs/otel-cli/otlpclient"
	"github.com/spf13/cobra"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// execCmd sets up the `otel-cli exec` command
func execCmd(config *Config) *cobra.Command {
	cmd := cobra.Command{
		Use:   "exec",
		Short: "execute the command provided",
		Long: `execute the command provided after the subcommand inside a span, measuring
and reporting how long it took to run. The wrapping span's w3c traceparent is automatically
passed to the child process's environment as TRACEPARENT.

Examples:

otel-cli exec -n my-cool-thing -s interesting-step curl https://cool-service/api/v1/endpoint

otel-cli exec -s "outer span" 'otel-cli exec -s "inner span" sleep 1'`,
		Run:  doExec,
		Args: cobra.MinimumNArgs(1),
	}

	addCommonParams(&cmd, config)
	addSpanParams(&cmd, config)
	addAttrParams(&cmd, config)
	addClientParams(&cmd, config)

	defaults := DefaultConfig()
	cmd.Flags().StringVar(
		&config.ExecCommandTimeout,
		"command-timeout",
		defaults.ExecCommandTimeout,
		"timeout for the child process, when 0 otel-cli will wait forever",
	)

	return &cmd
}

func doExec(cmd *cobra.Command, args []string) {
	ctx := cmd.Context()
	config := getConfig(ctx)

	// put the command in the attributes, before creating the span so it gets picked up
	config.Attributes["command"] = args[0]
	config.Attributes["arguments"] = ""

	// no deadline if there is no command timeout set
	cancelCtxDeadline := func() {}
	cmdCtx := ctx
	cmdTimeout := config.ParseExecCommandTimeout()
	if cmdTimeout > 0 {
		cmdCtx, cancelCtxDeadline = context.WithDeadline(ctx, time.Now().Add(cmdTimeout))
	}

	var child *exec.Cmd
	if len(args) > 1 {
		// CSV-join the arguments to send as an attribute
		buf := bytes.NewBuffer([]byte{})
		csv.NewWriter(buf).WriteAll([][]string{args[1:]})
		config.Attributes["arguments"] = buf.String()

		child = exec.CommandContext(cmdCtx, args[0], args[1:]...)
	} else {
		child = exec.CommandContext(cmdCtx, args[0])
	}

	// attach all stdio to the parent's handles
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	// pass the existing env but add the latest TRACEPARENT carrier so e.g.
	// otel-cli exec 'otel-cli exec sleep 1' will relate the spans automatically
	child.Env = []string{}

	// grab everything BUT the TRACEPARENT envvar
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "TRACEPARENT=") {
			child.Env = append(child.Env, env)
		}
	}

	span := config.NewProtobufSpan()

	// set the traceparent to the current span to be available to the child process
	if config.GetIsRecording() {
		tp := otlpclient.TraceparentFromProtobufSpan(span, config.GetIsRecording())
		child.Env = append(child.Env, fmt.Sprintf("TRACEPARENT=%s", tp.Encode()))
		// when not recording, and a traceparent is available, pass it through
	} else if !config.TraceparentIgnoreEnv {
		tp := config.LoadTraceparent()
		if tp.Initialized {
			child.Env = append(child.Env, fmt.Sprintf("TRACEPARENT=%s", tp.Encode()))
		}
	}

	// ctrl-c (sigint) is forwarded to the child process
	signals := make(chan os.Signal, 10)
	signalsDone := make(chan struct{})
	signal.Notify(signals, os.Interrupt)
	go func() {
		sig := <-signals
		child.Process.Signal(sig)
		// this might not seem necessary but without it, otel-cli exits before sending the span
		close(signalsDone)
	}()

	if err := child.Run(); err != nil {
		span.Status = &tracev1.Status{
			Message: fmt.Sprintf("exec command failed: %s", err),
			Code:    tracev1.Status_STATUS_CODE_ERROR,
		}
	}
	span.EndTimeUnixNano = uint64(time.Now().UnixNano())

	cancelCtxDeadline()
	close(signals)
	<-signalsDone

	// set --timeout on just the OTLP egress, starting now instead of process start time
	ctx, cancelCtxDeadline = context.WithDeadline(ctx, time.Now().Add(config.GetTimeout()))
	defer cancelCtxDeadline()

	ctx, client := StartClient(ctx, config)
	ctx, err := otlpclient.SendSpan(ctx, client, config, span)
	if err != nil {
		config.SoftFail("unable to send span: %s", err)
	}

	_, err = client.Stop(ctx)
	if err != nil {
		config.SoftFail("client.Stop() failed: %s", err)
	}

	// set the global exit code so main() can grab it and os.Exit() properly
	Diag.ExecExitCode = child.ProcessState.ExitCode()

	config.PropagateTraceparent(span, os.Stdout)
}
