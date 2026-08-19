//! An OpenAPI service registered with `contenox tools add`, driven against a
//! loopback stub this process runs. The stub listens on 127.0.0.1 and talks to
//! nothing, so these cases still need no network and no credentials — what they
//! need is a service whose received requests can be read back, which is the
//! only way to check a claim about what contenox puts on the wire.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn};
use serde_json::{Value, json};
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::Duration;

/// The stub, its port, and the journal of everything it was sent.
struct Ledger {
    child: Child,
    port: u16,
    journal: PathBuf,
}

impl Ledger {
    fn start(cx: &Instance, require_auth: bool) -> Ledger {
        let journal = cx.root().join("ledger-journal.jsonl");
        let mut command = Command::new(env!("CARGO_BIN_EXE_openapi_stub"));
        command.arg(&journal);
        if require_auth {
            command.arg("--require-auth");
        }
        let mut child = command
            .stdout(Stdio::piped())
            .spawn()
            .expect("spawn the loopback ledger");

        let mut line = String::new();
        BufReader::new(child.stdout.take().expect("the stub kept no stdout"))
            .read_line(&mut line)
            .expect("the stub announces its port");
        let port = line
            .trim()
            .strip_prefix("PORT=")
            .and_then(|port| port.parse().ok())
            .unwrap_or_else(|| panic!("the stub announced {line:?}"));

        Ledger {
            child,
            port,
            journal,
        }
    }

    fn url(&self) -> String {
        format!("http://127.0.0.1:{}", self.port)
    }

    /// Every request the service was sent, in order.
    fn received(&self) -> Vec<Value> {
        let text = std::fs::read_to_string(&self.journal).unwrap_or_default();
        text.lines()
            .filter(|line| !line.trim().is_empty())
            .map(|line| serde_json::from_str(line).expect("the journal holds JSON lines"))
            .collect()
    }

    /// Only the calls to the operation itself — the spec fetches are noise.
    fn calls(&self) -> Vec<Value> {
        self.received()
            .into_iter()
            .filter(|request| request["path"] == json!("/entries"))
            .collect()
    }
}

impl Drop for Ledger {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

/// A scratch instance whose model calls the ledger once and then reports back.
fn asks_the_ledger() -> Script {
    Script::new().turns([
        Turn::new()
            .text("Asking the ledger.")
            .call(ToolCall::new("list_entries").arg("since", "2026-01-01")),
        Turn::new().text("The ledger answered."),
    ])
}

fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(&asks_the_ledger())
        .expect("scripted-test backend");
    cx
}

/// The shipped flat chain, granted only the named toolsets.
fn chain_granting(cx: &Instance, toolsets: Value) -> PathBuf {
    let source = cx.home_file(".generated/chain-agent-acpx.json");
    let text = std::fs::read_to_string(&source).expect("the compiled chain");
    let mut chain: Value = serde_json::from_str(&text).expect("the compiled chain is JSON");
    for task in chain["tasks"].as_array_mut().expect("tasks").iter_mut() {
        if let Some(config) = task
            .get_mut("execute_config")
            .and_then(Value::as_object_mut)
        {
            config.insert("tools".into(), toolsets.clone());
        }
    }
    let at = cx.work().join("ledger-chain.json");
    std::fs::write(&at, chain.to_string()).expect("plant the chain");
    at
}

/// The editor shape with the permission gate answered for us, so what a case
/// sees is the service call rather than the card in front of it.
fn run_once(cx: &Instance, chain: &Path) -> String {
    let mut acp = Acp::spawn(
        cx.cmd(["acp", "--auto"])
            .env("CONTENOX_ACP_CHAIN_PATH", chain),
    )
    .expect("spawn the ACP surface");
    acp.timeout(Duration::from_secs(120));
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "ask the ledger").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");
    turn.tool_outputs()
}

// ---------------------------------------------------------------------------

#[test]
fn a_registered_service_becomes_a_toolset_the_model_can_call() {
    let cx = instance("remote-callable");
    let ledger = Ledger::start(&cx, false);
    cx.run(["tools", "add", "ledger", "--url", &ledger.url()])
        .ok()
        .expect_stdout("1 tool(s) discovered");

    let said = run_once(&cx, &chain_granting(&cx, json!(["ledger"])));

    assert!(
        said.contains("the ledger answered"),
        "the operation ran and its answer came back to the model:\n{said}"
    );
    assert_eq!(
        ledger.calls().len(),
        1,
        "the service was called exactly once, got {:?}",
        ledger.calls()
    );
    assert!(
        ledger.calls()[0]["query"]
            .as_str()
            .unwrap_or_default()
            .contains("since=2026-01-01"),
        "with the argument the model supplied: {:?}",
        ledger.calls()[0]
    );
}

#[test]
fn a_service_outside_the_tasks_allowlist_is_never_called() {
    let cx = instance("remote-not-granted");
    let ledger = Ledger::start(&cx, false);
    cx.run(["tools", "add", "ledger", "--url", &ledger.url()])
        .ok();

    let said = run_once(&cx, &chain_granting(&cx, json!(["local_fs"])));

    assert!(
        said.contains("tool list_entries not found"),
        "a registered service is still policy-scoped, and this task did not name it:\n{said}"
    );
    assert!(
        ledger.calls().is_empty(),
        "and nothing reached it, got {:?}",
        ledger.calls()
    );
}

#[test]
fn a_registered_header_is_sent_on_every_call_to_the_service() {
    let cx = instance("remote-header");
    let ledger = Ledger::start(&cx, false);
    cx.run([
        "tools",
        "add",
        "ledger",
        "--url",
        &ledger.url(),
        "--header",
        "X-Tenant: acme",
    ])
    .ok();

    run_once(&cx, &chain_granting(&cx, json!(["ledger"])));

    let calls = ledger.calls();
    assert!(!calls.is_empty(), "the service was called");
    for call in &calls {
        assert_eq!(
            call["headers"]["x-tenant"],
            json!("acme"),
            "every call carries the registered header, not just the first: {call:?}"
        );
    }
}

#[test]
#[ignore = "confirmed defect: `contenox tools add --inject key=value` is stored and shown but never sent. \
RemoteTools.InjectParams — documented in store.go as \"injected as tool call args, hidden from model schema\", \
and by `tools add --inject` as \"injects a named parameter into every tool call, hidden from the model\" — \
is read nowhere on the OpenAPI call path: remoteprovider.go builds injectParams from Properties (a single \
legacy InjectionArg the CLI never sets) and Headers only, in both GetToolsForToolsByName and the ExecuteTool \
seam. The MCP path does read srv.InjectParams, so the flag works there and silently does nothing here."]
fn an_injected_argument_reaches_the_service_without_the_model_supplying_it() {
    let cx = instance("remote-inject");
    let ledger = Ledger::start(&cx, false);
    cx.run([
        "tools",
        "add",
        "ledger",
        "--url",
        &ledger.url(),
        "--inject",
        "region=eu-west",
    ])
    .ok();

    run_once(&cx, &chain_granting(&cx, json!(["ledger"])));

    let calls = ledger.calls();
    assert!(!calls.is_empty(), "the service was called");
    assert!(
        calls[0]["query"]
            .as_str()
            .unwrap_or_default()
            .contains("region=eu-west"),
        "the injected argument is on the call the model never put it on: {:?}",
        calls[0]
    );
}

#[test]
fn a_service_that_answers_401_is_logged_into_and_the_call_is_retried() {
    let cx = instance("remote-login");
    let ledger = Ledger::start(&cx, true);
    cx.run([
        "tools",
        "add",
        "ledger",
        "--url",
        &ledger.url(),
        "--auth-login-url",
        &format!("{}/login", ledger.url()),
        "--auth-login-body",
        r#"{"user":"tester"}"#,
        "--auth-extract-jsonpath",
        "$.data.token",
        "--auth-inject-header",
        "Authorization",
        "--auth-inject-format",
        "Bearer %s",
    ])
    .ok();

    let said = run_once(&cx, &chain_granting(&cx, json!(["ledger"])));

    assert!(
        said.contains("the ledger answered"),
        "the call succeeded on the retry, and the model never saw the 401:\n{said}"
    );

    let seen: Vec<String> = ledger
        .received()
        .iter()
        .filter(|request| request["path"] != json!("/openapi.json"))
        .map(|request| request["path"].as_str().unwrap_or_default().to_string())
        .collect();
    assert_eq!(
        seen,
        vec!["/entries", "/login", "/entries"],
        "the refusal triggers the login and the original call is made again"
    );

    let calls = ledger.calls();
    assert!(
        calls[0]["headers"].get("authorization").is_none(),
        "the first attempt had no token: {:?}",
        calls[0]
    );
    assert_eq!(
        calls[1]["headers"]["authorization"],
        json!("Bearer issued-by-the-login-flow"),
        "and the second carries the token the login handed back, in the declared format"
    );
}

#[test]
fn a_service_the_hardened_envelope_does_not_know_is_refused_before_it_is_called() {
    let cx = instance("remote-envelope");
    let ledger = Ledger::start(&cx, false);
    cx.run(["tools", "add", "ledger", "--url", &ledger.url()])
        .ok();
    let chain = chain_granting(&cx, json!(["ledger"]));

    // acpx runs the hardened envelope, which allows what it names and no more.
    let mut acp = Acp::spawn(cx.cmd(["acpx"]).env("CONTENOX_ACPX_CHAIN_PATH", &chain))
        .expect("spawn the headless ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "ask the ledger").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");

    assert!(
        turn.tool_outputs()
            .contains("Denied by the active policy hitl-policy-acpx.json"),
        "a registered service is a policy-scoped tool like any other:\n{}",
        turn.tool_outputs()
    );
    assert!(
        ledger.calls().is_empty(),
        "and the envelope refuses before anything reaches it, got {:?}",
        ledger.calls()
    );
}
