//! Tools: which ones exist at all.
//!
//! Before a tool can cross the boundary it has to be in the task's allowlist,
//! and the model has to have been told about it. These cases pin the allowlist
//! vocabulary, the `{{tools}}` manifest that is built from it, and the two ways
//! an operator adds a tool that is not built in — an MCP server and an OpenAPI
//! service.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn};
use serde_json::{Value, json};
use std::path::PathBuf;

fn instance(label: &str, script: &Script) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(script).expect("scripted-test backend");
    cx
}

// ------------------------------------------------- the allowlist vocabulary

/// The shipped flat chain with every task's `tools` allowlist rewritten —
/// `None` removes the key, which is the "absent" case of the vocabulary.
fn chain_with_tools(cx: &Instance, allowlist: Option<Value>) -> PathBuf {
    let source = cx.home_file(".generated/chain-agent-acpx.json");
    let text = std::fs::read_to_string(&source)
        .unwrap_or_else(|err| panic!("read {}: {err}", source.display()));
    let mut chain: Value = serde_json::from_str(&text).expect("the compiled chain is JSON");
    for task in chain["tasks"]
        .as_array_mut()
        .expect("the chain holds tasks")
        .iter_mut()
    {
        let Some(config) = task
            .get_mut("execute_config")
            .and_then(Value::as_object_mut)
        else {
            continue;
        };
        match &allowlist {
            Some(list) => {
                config.insert("tools".into(), list.clone());
            }
            None => {
                config.remove("tools");
            }
        }
    }
    let at = cx.work().join("allowlist-chain.json");
    std::fs::write(&at, chain.to_string()).expect("plant the chain");
    at
}

/// A turn that reaches for one tool from `local_fs` and one from
/// `native-fs-browse`, so a case can see each toolset's fate separately.
fn reaches_for_both() -> Script {
    Script::new().turns([
        Turn::new()
            .text("Looking around.")
            .call(ToolCall::new("read_file").arg("path", "note.txt"))
            .call(ToolCall::new("list_dir").arg("path", ".")),
        Turn::new().text("That is what I found."),
    ])
}

/// Run one prompt through the planted chain and hand back what the tools said.
fn tool_outputs_under(cx: &Instance, chain: &PathBuf) -> String {
    let mut acp = Acp::spawn(cx.cmd(["acpx"]).env("CONTENOX_ACPX_CHAIN_PATH", chain))
        .expect("spawn the headless ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp.prompt(&session, "look around").expect("prompt");
    acp.close()
        .expect("the agent exits when its client hangs up");
    turn.tool_outputs()
}

fn reached(said: &str, tool: &str) -> bool {
    !said.contains(&format!("tool {tool} not found"))
}

#[test]
fn a_task_with_no_tools_line_at_all_may_call_nothing() {
    let cx = instance("allow-absent", &reaches_for_both());
    cx.write_file("note.txt", "note\n").expect("write the note");
    let chain = chain_with_tools(&cx, None);

    let said = tool_outputs_under(&cx, &chain);
    assert!(
        !reached(&said, "read_file") && !reached(&said, "list_dir"),
        "an absent allowlist grants nothing, not everything:\n{said}"
    );
}

#[test]
fn an_empty_tools_list_may_call_nothing() {
    let cx = instance("allow-empty", &reaches_for_both());
    cx.write_file("note.txt", "note\n").expect("write the note");
    let chain = chain_with_tools(&cx, Some(json!([])));

    let said = tool_outputs_under(&cx, &chain);
    assert!(
        !reached(&said, "read_file") && !reached(&said, "list_dir"),
        "an empty list reads the same as no list:\n{said}"
    );
}

#[test]
fn a_star_grants_every_toolset() {
    let cx = instance("allow-star", &reaches_for_both());
    cx.write_file("note.txt", "note\n").expect("write the note");
    let chain = chain_with_tools(&cx, Some(json!(["*"])));

    let said = tool_outputs_under(&cx, &chain);
    assert!(
        reached(&said, "read_file") && reached(&said, "list_dir"),
        "a star is every registered toolset:\n{said}"
    );
}

#[test]
fn naming_toolsets_grants_those_and_leaves_the_rest_out() {
    let cx = instance("allow-named", &reaches_for_both());
    cx.write_file("note.txt", "note\n").expect("write the note");
    let chain = chain_with_tools(&cx, Some(json!(["local_fs"])));

    let said = tool_outputs_under(&cx, &chain);
    assert!(
        reached(&said, "read_file"),
        "the named toolset is granted:\n{said}"
    );
    assert!(!reached(&said, "list_dir"), "and nothing else is:\n{said}");
}

#[test]
fn a_star_with_an_exclusion_grants_all_but_the_excluded_toolset() {
    let cx = instance("allow-star-except", &reaches_for_both());
    cx.write_file("note.txt", "note\n").expect("write the note");
    let chain = chain_with_tools(&cx, Some(json!(["*", "!local_fs"])));

    let said = tool_outputs_under(&cx, &chain);
    assert!(
        !reached(&said, "read_file"),
        "the exclusion holds against the star:\n{said}"
    );
    assert!(
        reached(&said, "list_dir"),
        "and everything else is still granted:\n{said}"
    );
}

#[test]
fn an_exclusion_beats_the_star_that_follows_it_as_well_as_the_one_before() {
    let cx = instance("allow-except-star", &reaches_for_both());
    cx.write_file("note.txt", "note\n").expect("write the note");
    let chain = chain_with_tools(&cx, Some(json!(["!local_fs", "*"])));

    let said = tool_outputs_under(&cx, &chain);
    assert!(
        !reached(&said, "read_file"),
        "order does not rescue an excluded toolset:\n{said}"
    );
    assert!(
        reached(&said, "list_dir"),
        "and the star still grants the rest:\n{said}"
    );
}

// --------------------------------------------------------- the {{tools}} macro

/// The manifest as it reached the model, parsed out of the expanded prompt
/// `contenox state show --raw` records.
fn manifest(cx: &Instance) -> Value {
    let prompts = cx
        .captured_system_prompts()
        .expect("contenox state show --raw");
    let prompt = prompts
        .iter()
        .find(|text| text.contains("Available tools"))
        .unwrap_or_else(|| panic!("no prompt carried a tool manifest, got {prompts:?}"));
    let opened = prompt
        .find("Available tools (tools -> function names):\n")
        .expect("the manifest is introduced by its own line");
    let json_start = prompt[opened..]
        .find('{')
        .map(|offset| opened + offset)
        .expect("the manifest is a JSON object");
    let json_end = prompt[json_start..]
        .find("\n")
        .map(|offset| json_start + offset)
        .unwrap_or(prompt.len());
    serde_json::from_str(&prompt[json_start..json_end]).unwrap_or_else(|err| {
        panic!(
            "the manifest did not parse: {err}\n{}",
            &prompt[json_start..]
        )
    })
}

#[test]
fn the_tools_macro_renders_the_live_roster_rather_than_a_written_out_list() {
    let cx = instance(
        "macro-live",
        &Script::new().turn(Turn::new().text("Nothing to do.")),
    );
    let chain = chain_with_tools(&cx, Some(json!(["*"])));
    tool_outputs_under(&cx, &chain);

    let manifest = manifest(&cx);
    assert_eq!(
        manifest["local_fs"],
        json!([
            "edit_file",
            "read_file",
            "read_file_range",
            "sed",
            "write_file"
        ]),
        "the macro names the tools each toolset actually has, not a copy in the prompt"
    );
    assert_eq!(manifest["local_shell"], json!(["local_shell"]));
    assert!(
        manifest.get("native-git").is_some(),
        "and every other registered toolset: {manifest:#}"
    );
}

#[test]
fn the_tools_macro_shows_only_what_the_tasks_allowlist_admits() {
    let cx = instance(
        "macro-allowlist",
        &Script::new().turn(Turn::new().text("Nothing to do.")),
    );
    let chain = chain_with_tools(&cx, Some(json!(["local_fs"])));
    tool_outputs_under(&cx, &chain);

    let manifest = manifest(&cx);
    assert_eq!(
        manifest.as_object().map(|fields| fields.len()),
        Some(1),
        "the manifest is the allowlist's own view, got {manifest:#}"
    );
    assert!(
        manifest.get("local_fs").is_some(),
        "and it is the toolset the task named: {manifest:#}"
    );
}

#[test]
fn a_task_that_may_call_nothing_is_told_so_rather_than_shown_the_whole_roster() {
    let cx = instance(
        "macro-empty",
        &Script::new().turn(Turn::new().text("Nothing to do.")),
    );
    let chain = chain_with_tools(&cx, Some(json!([])));
    tool_outputs_under(&cx, &chain);

    assert_eq!(
        manifest(&cx),
        json!({}),
        "an empty allowlist renders an empty manifest, never the full roster"
    );
}

// ----------------------------------------------------------- MCP registration

fn mcp_show(cx: &Instance, name: &str) -> Value {
    let out = cx.run(["mcp", "show", name]).ok();
    serde_json::from_str(&out.stdout)
        .unwrap_or_else(|err| panic!("contenox mcp show {name} is JSON: {err}\n{}", out.render()))
}

#[test]
fn an_mcp_server_joins_the_roster_as_a_source_served_live_per_session() {
    let cx = Instance::named("mcp-roster").expect("scratch instance");
    cx.init().ok();
    cx.run([
        "mcp",
        "add",
        "ledger",
        "--transport",
        "stdio",
        "--command",
        "ledger-mcp",
    ])
    .ok();

    let roster = cx.doctor().ok().stdout;
    assert!(
        roster.contains(
            "ledger — MCP server (stdio ledger-mcp); its tools are served live per session"
        ),
        "an MCP server is named in the roster as its own source:\n{roster}"
    );
}

#[test]
fn mcp_show_prints_the_keys_of_headers_and_injects_and_never_their_values() {
    let cx = Instance::named("mcp-redaction").expect("scratch instance");
    cx.init().ok();
    cx.run([
        "mcp",
        "add",
        "ledger",
        "https://ledger.invalid/mcp",
        "--header",
        "Authorization: Bearer super-secret",
        "--inject",
        "tenant_id=acme",
    ])
    .ok();

    let shown = mcp_show(&cx, "ledger");
    assert_eq!(shown["headers"]["Authorization"], json!("(hidden)"));
    assert_eq!(shown["injectParams"]["tenant_id"], json!("(hidden)"));

    let raw = cx.run(["mcp", "show", "ledger"]).ok();
    assert!(
        !raw.stdout.contains("super-secret") && !raw.stdout.contains("acme"),
        "a secret never reaches the screen:\n{}",
        raw.stdout
    );
}

#[test]
fn mcp_update_header_replaces_the_whole_map_rather_than_merging_into_it() {
    let cx = Instance::named("mcp-header-replace").expect("scratch instance");
    cx.init().ok();
    cx.run([
        "mcp",
        "add",
        "ledger",
        "https://ledger.invalid/mcp",
        "--header",
        "Authorization: Bearer one",
        "--header",
        "X-Tenant: acme",
    ])
    .ok();
    cx.run(["mcp", "update", "ledger", "--header", "X-Tenant: beta"])
        .ok();

    let headers = mcp_show(&cx, "ledger")["headers"].clone();
    assert_eq!(
        headers,
        json!({"X-Tenant": "(hidden)"}),
        "the flag replaces the set, so a dropped header is really gone"
    );
}

#[test]
fn mcp_update_inject_replaces_the_whole_map_rather_than_merging_into_it() {
    let cx = Instance::named("mcp-inject-replace").expect("scratch instance");
    cx.init().ok();
    cx.run([
        "mcp",
        "add",
        "ledger",
        "https://ledger.invalid/mcp",
        "--inject",
        "tenant_id=acme",
        "--inject",
        "region=eu",
    ])
    .ok();
    cx.run(["mcp", "update", "ledger", "--inject", "region=us"])
        .ok();

    assert_eq!(
        mcp_show(&cx, "ledger")["injectParams"],
        json!({"region": "(hidden)"}),
        "the flag replaces the set, so a dropped param is really gone"
    );
}

#[test]
fn mcp_update_cannot_move_a_server_to_another_transport_command_or_url() {
    let cx = Instance::named("mcp-immutable-target").expect("scratch instance");
    cx.init().ok();
    cx.run([
        "mcp",
        "add",
        "ledger",
        "--transport",
        "stdio",
        "--command",
        "ledger-mcp",
    ])
    .ok();

    for flag in ["--transport", "--command", "--url"] {
        cx.run(["mcp", "update", "ledger", flag, "elsewhere"])
            .expect_failure()
            .expect_stderr("unknown flag");
    }
    cx.run(["mcp", "update", "ledger", "--args", "a,b"])
        .expect_failure()
        .expect_stderr("unknown flag");

    let shown = mcp_show(&cx, "ledger");
    assert_eq!(shown["transport"], json!("stdio"));
    assert_eq!(shown["command"], json!("ledger-mcp"));
}

// ------------------------------------------------------- OpenAPI registration

#[test]
fn an_openapi_service_joins_the_roster_as_a_policy_scoped_source_of_its_own() {
    let cx = Instance::named("openapi-roster").expect("scratch instance");
    cx.init().ok();
    cx.run(["tools", "add", "ledger", "--url", "https://ledger.invalid"])
        .ok();

    let listed = cx.run(["tools", "list"]).ok();
    assert!(
        listed.stdout.contains("ledger") && listed.stdout.contains("https://ledger.invalid"),
        "the provider is registered under its own name:\n{}",
        listed.stdout
    );
}

#[test]
fn the_login_flow_can_only_be_set_when_the_service_is_registered() {
    let cx = Instance::named("openapi-auth-add-time").expect("scratch instance");
    cx.init().ok();
    cx.run([
        "tools",
        "add",
        "ledger",
        "--url",
        "https://ledger.invalid",
        "--auth-login-url",
        "https://ledger.invalid/login",
        "--auth-extract-jsonpath",
        "$.data.token",
        "--auth-inject-header",
        "Authorization",
    ])
    .ok();

    for flag in [
        "--auth-login-url",
        "--auth-extract-cookie",
        "--auth-inject-header",
        "--insecure-skip-tls-verify",
    ] {
        cx.run(["tools", "update", "ledger", flag, "x"])
            .expect_failure()
            .expect_stderr("unknown flag");
    }
    assert!(
        cx.run(["tools", "update", "--help"])
            .ok()
            .stdout
            .contains("can\nonly be set at registration time")
            || cx
                .run(["tools", "update", "--help"])
                .ok()
                .stdout
                .contains("only be set at registration time"),
        "and the help says where those flags live instead"
    );
}
