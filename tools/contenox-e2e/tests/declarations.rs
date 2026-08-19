//! Declarations and reach — what a Markdown file with a YAML header buys, and
//! what the operator is told about the parts of it that were not carried.
//!
//! Every case here drops files into a scratch workspace and then asks the
//! product what it made of them: the roster (`contenox agent list`), the
//! registries the declaration wrote into (`contenox mcp list`,
//! `contenox tools list`), the artefacts `contenox agent show` points at under
//! `.generated/`, and — where reach is only real if a call actually runs — the
//! transcript of a run, read back with `contenox session show`.

use contenox_e2e::{Instance, Script, ToolCall, Turn};
use serde_json::Value;
use std::time::Duration;

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx
}

/// One discovery pass, and everything it could not act on. Every command that
/// touches the roster runs the pass first and prints its problems on stderr;
/// `agent list` is the one an operator reaches for.
fn discover(cx: &Instance) -> contenox_e2e::CmdOutput {
    cx.run(["agent", "list"])
}

fn roster(cx: &Instance) -> Vec<String> {
    let out = discover(cx).ok();
    contenox_e2e::Table::parse(&out.stdout, &["ID", "NAME", "SOURCE", "KIND", "ENABLED"])
        .expect("contenox agent list prints its table")
        .rows
        .iter()
        .map(|row| row.get("NAME").to_string())
        .collect()
}

fn enabled(cx: &Instance, name: &str) -> Option<bool> {
    let out = discover(cx).ok();
    contenox_e2e::Table::parse(&out.stdout, &["ID", "NAME", "SOURCE", "KIND", "ENABLED"])
        .expect("contenox agent list prints its table")
        .rows
        .iter()
        .find(|row| row.get("NAME") == name)
        .map(|row| row.get("ENABLED") == "true")
}

/// The chain `contenox agent show <name>` points at, parsed. Reading it is
/// reading a file the product told us it wrote, not reaching into its insides.
fn emitted_chain(cx: &Instance, agent: &str) -> Value {
    let shown = cx.run(["agent", "show", agent]).ok();
    let path = cx.generated(&format!("chain-agent-{agent}.json"));
    assert!(
        shown.stdout.contains(&path.display().to_string()),
        "contenox agent show {agent} must point at {}: {}",
        path.display(),
        shown.render()
    );
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|err| panic!("read {}: {err}", path.display()));
    serde_json::from_str(&text).expect("the emitted chain is JSON")
}

fn emitted_policy(cx: &Instance, agent: &str) -> Value {
    let path = cx.generated(&format!("hitl-policy-{agent}.json"));
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|err| panic!("read {}: {err}", path.display()));
    serde_json::from_str(&text).expect("the emitted policy is JSON")
}

fn task<'a>(chain: &'a Value, id: &str) -> &'a Value {
    chain["tasks"]
        .as_array()
        .expect("a chain has tasks")
        .iter()
        .find(|t| t["id"] == id)
        .unwrap_or_else(|| panic!("no task {id:?} in {chain:#}"))
}

fn task_ids(chain: &Value) -> Vec<String> {
    chain["tasks"]
        .as_array()
        .expect("a chain has tasks")
        .iter()
        .map(|t| t["id"].as_str().unwrap_or_default().to_string())
        .collect()
}

/// The toolsets the agent's own turn is granted.
fn granted(chain: &Value, agent_task: &str) -> Vec<String> {
    task(chain, agent_task)["execute_config"]["tools"]
        .as_array()
        .map(|list| {
            list.iter()
                .map(|v| v.as_str().unwrap_or_default().to_string())
                .collect()
        })
        .unwrap_or_default()
}

fn hidden(chain: &Value, agent_task: &str) -> Vec<String> {
    task(chain, agent_task)["execute_config"]["hide_tools"]
        .as_array()
        .map(|list| {
            list.iter()
                .map(|v| v.as_str().unwrap_or_default().to_string())
                .collect()
        })
        .unwrap_or_default()
}

/// The dialog a run needs to land after trying one tool: call it, file a
/// result, finish, then answer the loop's last question.
fn tries(call: ToolCall) -> Script {
    Script::new()
        .turn(Turn::new().text("Trying it.").call(call))
        .turn(
            Turn::new()
                .text("Filing.")
                .call(ToolCall::mission_report("result", "the probe ran")),
        )
        .turn(Turn::new().call(ToolCall::mission_finish("landed")))
        .turn("Mission finished.")
}

/// Fire one declared agent as an unattended run and hand back the conversation
/// the product recorded for it. A tool result — the listing, or
/// `tool <name> not found` — is only visible here, and `contenox session show`
/// is the command that prints it.
fn transcript_of_one_run(cx: &Instance, agent: &str, intent: &str) -> String {
    cx.cmd(["run", agent, intent, "--policy", "run"])
        .timeout(Duration::from_secs(240))
        .output()
        .expect("contenox run")
        .ok();
    let sessions = cx.sessions_all().expect("contenox session list --all");
    assert_eq!(
        sessions.len(),
        1,
        "one run should leave exactly one session, got {sessions:?}"
    );
    cx.session_show(&sessions[0].id).ok().stdout
}

// ---------------------------------------------------------------------------
// An agent is one file
// ---------------------------------------------------------------------------

/// Drop it in and the next run picks it up: no register step, no build step,
/// no restart.
#[test]
fn a_declaration_dropped_into_the_home_agents_directory_needs_no_build_step() {
    let cx = instance("decl-dropin");

    assert!(
        !roster(&cx).contains(&"dropin".to_string()),
        "the agent must not exist before its file does"
    );

    cx.write_home_file(
        "agents/dropin.md",
        "---\nname: dropin\ndescription: Dropped in with no build step\ntools: Read\n---\n\
         You are a probe. Answer briefly.\n",
    )
    .expect("write the declaration");

    assert!(
        roster(&cx).contains(&"dropin".to_string()),
        "the next discovering command must pick the file up"
    );
    assert_eq!(
        granted(&emitted_chain(&cx, "dropin"), "dropin-agent"),
        vec!["local_fs", "mission"],
        "and transpile it into a chain"
    );
}

/// A workspace declaration works the same way, and keeps its own name.
#[test]
fn a_workspace_declaration_keeps_the_name_it_declared() {
    let cx = instance("decl-workspace");
    cx.write_file(
        ".contenox/agents/mine.md",
        "---\nname: mine\ndescription: My own agent\ntools: Read\n---\nBody.\n",
    )
    .expect("write the declaration");

    assert!(roster(&cx).contains(&"mine".to_string()));
}

// ---------------------------------------------------------------------------
// Directories other tools own are read where they are
// ---------------------------------------------------------------------------

#[test]
fn a_claude_agents_declaration_is_read_in_place_and_prefixed_with_the_tool_it_came_from() {
    let cx = instance("decl-claude-dir");
    cx.write_file(
        ".claude/agents/reviewer.md",
        "---\nname: reviewer\ndescription: Reviews a file for correctness problems\ntools: Read\n\
         ---\nYou are a code reviewer.\n",
    )
    .expect("write the declaration");

    let names = roster(&cx);
    assert!(
        names.contains(&"claude-code-reviewer".to_string()),
        "an imported agent carries the tool it came from: {names:?}"
    );
    assert!(
        names.contains(&"reviewer".to_string()),
        "and the shipped reviewer declaration keeps its own bare name: {names:?}"
    );
}

#[test]
fn an_agents_directory_declaration_naming_its_dialect_is_prefixed_with_it() {
    let cx = instance("decl-agents-dir-dialect");
    cx.write_file(
        ".agents/agents/pathfinder.md",
        "---\nname: pathfinder\ndescription: Finds paths\ntools: Read\nmainAgent: true\n---\n\
         You find paths.\n",
    )
    .expect("write the declaration");

    assert!(
        roster(&cx).contains(&"antigravity-pathfinder".to_string()),
        "a declaration carrying a field unique to one product is read as that product's"
    );
}

/// `.agents/agents/` is documented as read where it is, exactly like
/// `.claude/agents/`.
#[test]
#[ignore = "confirmed defect: docs/guide/agents.md promises `.agents/agents/` is read where it is, \
            but agentdecl.DetectDialect's `.agents` anchor is neutered by its own guard — it only \
            fires for `agent.md` or a file already carrying an antigravity fingerprint, both of \
            which the later rules match anyway. An ordinary declaration there is refused as an \
            ambiguous dialect. Seam: internal/services/agentdecl/dialect.go, the \
            `a.segments[0] == \".agents\"` continue."]
fn an_agents_directory_declaration_is_read_where_it_is() {
    let cx = instance("decl-agents-dir");
    cx.write_file(
        ".agents/agents/scout.md",
        "---\nname: scout\ndescription: Scouts the tree\ntools: Read\n---\nYou are a scout.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        !out.stderr.contains("cannot tell which product"),
        "a declaration in a known agents directory must not be refused as ambiguous:\n{}",
        out.render()
    );
    assert!(
        roster(&cx).iter().any(|name| name.ends_with("-scout")),
        "and it lands on the roster prefixed with the tool it came from"
    );
}

// ---------------------------------------------------------------------------
// `tools:` admits whole toolsets
// ---------------------------------------------------------------------------

/// Omitting the line inherits everything — including the namespaced toolsets.
/// A `native-` prefix is a namespace, never a hidden exclusion, so an agent
/// that named no tool at all still reaches one it never heard of.
#[test]
fn omitting_the_tools_line_reaches_a_toolset_the_declaration_never_named() {
    let cx = instance("decl-inherit-reaches");
    cx.write_file(
        ".contenox/agents/reacher.md",
        "---\nname: reacher\ndescription: Names no tools at all\n---\nYou reach everything.\n",
    )
    .expect("write the declaration");
    cx.write_file("inherit-marker.txt", "the listing must show this file\n")
        .expect("write the marker");
    cx.scripted(&tries(ToolCall::new("list_dir").arg("path", ".")))
        .expect("scripted-test backend");

    assert_eq!(
        granted(&emitted_chain(&cx, "reacher"), "reacher-agent"),
        vec!["*"],
        "an omitted line is the inherit-everything token"
    );

    let said = transcript_of_one_run(&cx, "reacher", "look at the tree");
    assert!(
        said.contains("inherit-marker.txt"),
        "native-fs-browse.list_dir must have run and returned the listing:\n{said}"
    );
    assert!(
        !said.contains("tool list_dir not found"),
        "inheriting must not quietly skip a namespaced toolset:\n{said}"
    );
}

/// A bare name grants that toolset and stops there. The same run, the same
/// tool, an agent that named `Read` instead of nothing: the call never lands.
#[test]
fn naming_one_toolset_grants_that_toolset_and_nothing_else() {
    let cx = instance("decl-narrow-reach");
    cx.write_file(
        ".contenox/agents/narrow.md",
        "---\nname: narrow\ndescription: Only the toolset Read resolves to\ntools: Read\n---\n\
         You reach only files.\n",
    )
    .expect("write the declaration");
    cx.scripted(&tries(ToolCall::new("list_dir").arg("path", ".")))
        .expect("scripted-test backend");

    assert_eq!(
        granted(&emitted_chain(&cx, "narrow"), "narrow-agent"),
        vec!["local_fs", "mission"],
        "Read resolves to a tool in local_fs, which admits the toolset"
    );

    let said = transcript_of_one_run(&cx, "narrow", "look at the tree");
    assert!(
        said.contains("tool list_dir not found"),
        "a toolset the declaration did not name must not be reachable:\n{said}"
    );
}

#[test]
fn a_quoted_star_grants_what_omitting_the_line_grants() {
    let cx = instance("decl-star");
    cx.write_file(
        ".contenox/agents/said-out-loud.md",
        "---\nname: said-out-loud\ndescription: The star, said out loud\ntools: \"*\"\n---\nBody.\n",
    )
    .expect("write the declaration");
    cx.write_file(
        ".contenox/agents/left-unsaid.md",
        "---\nname: left-unsaid\ndescription: The same thing, unsaid\n---\nBody.\n",
    )
    .expect("write the declaration");
    discover(&cx).ok();

    for agent in ["said-out-loud", "left-unsaid"] {
        let sets = granted(&emitted_chain(&cx, agent), &format!("{agent}-agent"));
        assert!(
            sets.contains(&"*".to_string()),
            "{agent} must carry the inherit-everything token: {sets:?}"
        );
        assert!(
            !sets.iter().any(|set| set != "*" && set != "mission"),
            "and nothing that would narrow it: {sets:?}"
        );
    }
}

#[test]
fn an_empty_tools_list_grants_nothing() {
    let cx = instance("decl-empty-tools");
    cx.write_file(
        ".contenox/agents/nothing.md",
        "---\nname: nothing\ndescription: An empty list grants nothing\ntools: []\n---\nBody.\n",
    )
    .expect("write the declaration");

    let sets = granted(&emitted_chain(&cx, "nothing"), "nothing-agent");
    assert_eq!(
        sets,
        vec!["mission"],
        "nothing but the mission channel every unattended unit holds: {sets:?}"
    );
}

/// `!name` belongs to the chain allowlist, not to a declaration. In a
/// declaration it is not an exclusion — it resolves to nothing, is dropped
/// with the rest of the unresolved names, and the star it sat beside survives
/// untouched.
#[test]
fn a_bang_name_in_a_declaration_resolves_to_nothing_and_is_dropped() {
    let cx = instance("decl-bang");
    cx.write_file(
        ".contenox/agents/banged.md",
        "---\nname: banged\ndescription: Tries the chain allowlist's vocabulary\n\
         tools: [\"*\", \"!local_shell\"]\n---\nBody.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains("not carried  banged: tools"),
        "the drop is reported, not silent:\n{}",
        out.render()
    );

    let sets = granted(&emitted_chain(&cx, "banged"), "banged-agent");
    assert!(
        sets.contains(&"*".to_string()),
        "the star survives: {sets:?}"
    );
    assert!(
        !sets.iter().any(|s| s.starts_with('!')),
        "and the bang name reaches the chain as nothing at all: {sets:?}"
    );
}

/// The two halves work at different grains: `tools:` admits the toolset,
/// `disallowedTools:` hides one tool out of it. The sibling tool still runs.
#[test]
fn disallowed_tools_hides_one_tool_out_of_the_toolset_that_tools_admitted() {
    let cx = instance("decl-disallowed");
    cx.write_file(
        ".contenox/agents/hidden.md",
        "---\nname: hidden\ndescription: Holds the browse toolset with one tool hidden\n\
         tools: [\"native-fs-browse\"]\n\
         disallowedTools: [\"native-fs-browse.list_dir\"]\n---\nYou browse.\n",
    )
    .expect("write the declaration");
    cx.write_file("hidden-marker.txt", "stat me\n")
        .expect("write the marker");

    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Listing.")
                    .call(ToolCall::new("list_dir").arg("path", ".")),
            )
            .turn(
                Turn::new()
                    .text("Stat instead.")
                    .call(ToolCall::new("stat_file").arg("path", "hidden-marker.txt")),
            )
            .turn(
                Turn::new()
                    .text("Filing.")
                    .call(ToolCall::mission_report("result", "the probe ran")),
            )
            .turn(Turn::new().call(ToolCall::mission_finish("landed")))
            .turn("Mission finished."),
    )
    .expect("scripted-test backend");

    let chain = emitted_chain(&cx, "hidden");
    assert!(
        granted(&chain, "hidden-agent").contains(&"native-fs-browse".to_string()),
        "the toolset stays admitted"
    );
    assert_eq!(
        hidden(&chain, "hidden-agent"),
        vec!["native-fs-browse.list_dir"],
        "and the single tool is hidden out of it"
    );

    let said = transcript_of_one_run(&cx, "hidden", "browse the tree");
    assert!(
        said.contains("tool native-fs-browse.list_dir is hidden"),
        "the hidden tool is refused by name:\n{said}"
    );
    assert!(
        said.contains("hidden-marker.txt"),
        "and its sibling in the same toolset still runs:\n{said}"
    );
}

// ---------------------------------------------------------------------------
// What happens to a name that resolves to nothing
// ---------------------------------------------------------------------------

#[test]
fn an_unknown_tool_name_is_dropped_and_the_agent_keeps_the_rest() {
    let cx = instance("decl-unknown-tool");
    cx.write_file(
        ".contenox/agents/partial.md",
        "---\nname: partial\ndescription: Names one tool that is not connected\n\
         tools: Read, WebSearch\n---\nBody.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains(
            "not carried  partial: tools — resolves to nothing connected here; the agent runs \
             without it. Connect it and name it under [tools] in agents.toml"
        ),
        "the operator is told, and told what to do about it:\n{}",
        out.render()
    );
    assert!(roster(&cx).contains(&"partial".to_string()));
    assert_eq!(
        granted(&emitted_chain(&cx, "partial"), "partial-agent"),
        vec!["local_fs", "mission"],
        "the agent runs with what did resolve"
    );
}

/// The report has to name the tool it dropped. A declaration that lists eight
/// names and is told only that "tools" did not carry leaves the operator to
/// bisect the file by hand.
#[test]
#[ignore = "confirmed defect: the report drops the unmapped VALUE. \
            agentdecl.Config.MapTools records Unmapped{Field: \"tools\", Value: name, …}, but \
            printSyncProblems formats only Name/Field/Reason, so `WebSearch` never reaches the \
            operator. docs/guide/agents.md documents the line as `not carried  tools: WebSearch \
            …`. Seam: internal/surfaces/contenoxcli/chain_agent_discovery.go, printSyncProblems."]
fn the_report_names_the_tool_it_dropped() {
    let cx = instance("decl-unknown-tool-named");
    cx.write_file(
        ".contenox/agents/partial.md",
        "---\nname: partial\ndescription: Names one tool that is not connected\n\
         tools: Read, WebSearch\n---\nBody.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains("WebSearch"),
        "the dropped name is the one thing the operator cannot work out for themselves:\n{}",
        out.render()
    );
}

#[test]
fn a_declaration_where_no_tool_resolves_is_refused_and_names_every_unresolved_one() {
    let cx = instance("decl-no-tool-resolves");
    let path = cx
        .write_file(
            ".contenox/agents/lost.md",
            "---\nname: lost\ndescription: Nothing it names is connected\n\
             tools: [\"WebSearch\", \"Nonexistent\"]\n---\nBody.\n",
        )
        .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains(&format!("refused  {}", path.display())),
        "the refusal names the file:\n{}",
        out.render()
    );
    assert!(
        out.stderr.contains(
            "none of this declaration's tools resolve to anything connected here: WebSearch, \
             Nonexistent"
        ),
        "and every unresolved name, not just the first:\n{}",
        out.render()
    );
    assert!(
        !roster(&cx).contains(&"lost".to_string()),
        "a refused declaration registers nothing"
    );
}

/// The guide lists the names that resolve without connecting anything.
#[test]
#[ignore = "confirmed defect: docs/guide/agents.md says `Read, Write, Edit, Bash, PowerShell, \
            Glob, Grep, WebFetch` resolve out of the box, and its own opening example writes \
            `tools: Read, Glob, Grep` — but the shipped [tools] table in \
            internal/services/agentdecl/agents.toml maps only Read/Write/Edit/Bash/PowerShell \
            (plus three foreign spellings). Glob, Grep and WebFetch are dropped and reported, \
            though native-fs-browse.find_files, native-fs-browse.grep and native-web.web_get are \
            all mounted in-process. Seam: the [tools] table in agents.toml."]
fn the_tool_names_the_guide_promises_out_of_the_box_all_resolve() {
    let cx = instance("decl-out-of-the-box");
    cx.write_file(
        ".contenox/agents/documented.md",
        "---\nname: documented\ndescription: The names the guide says resolve\n\
         tools: Read, Write, Edit, Bash, PowerShell, Glob, Grep, WebFetch\n---\nBody.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        !out.stderr.contains("not carried  documented: tools"),
        "no documented name should be dropped:\n{}",
        out.render()
    );
}

// ---------------------------------------------------------------------------
// Tools an agent brings with it
// ---------------------------------------------------------------------------

const BRINGS_TWO_SOURCES: &str = "---
name: researcher2
description: Researches a question against internal sources
tools: Read
mcpServers:
  filesystem:
    command: npx
    args: [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/data\"]
  linear:
    type: http
    url: https://mcp.linear.app/mcp
    authEnvKey: LINEAR_TOKEN
remoteTools:
  billing:
    url: https://internal.example.com
    spec: https://internal.example.com/openapi.json
---
Body.
";

/// Naming the source is the grant. `tools:` says `Read` and nothing else, and
/// the agent still holds all three sources it brought.
#[test]
fn naming_a_source_in_the_declaration_is_the_grant_without_a_tools_entry() {
    let cx = instance("decl-brings-grant");
    cx.write_file(".contenox/agents/researcher2.md", BRINGS_TWO_SOURCES)
        .expect("write the declaration");

    let sets = granted(&emitted_chain(&cx, "researcher2"), "researcher2-agent");
    assert_eq!(
        sets,
        vec![
            "local_fs",
            "mission",
            "decl-researcher2-filesystem",
            "decl-researcher2-linear",
            "decl-researcher2-billing",
        ],
        "the tools line never mentions them; declaring them is the grant"
    );
}

#[test]
fn a_mapping_registers_sources_scoped_to_the_agent_and_owned_by_the_declaration() {
    let cx = instance("decl-brings-register");
    cx.write_file(".contenox/agents/researcher2.md", BRINGS_TWO_SOURCES)
        .expect("write the declaration");
    discover(&cx).ok();

    let servers = cx.run(["mcp", "list"]).ok();
    let table = contenox_e2e::Table::parse(
        &servers.stdout,
        &["NAME", "TRANSPORT", "COMMAND/URL", "OWNER"],
    )
    .expect("contenox mcp list prints its table");
    let stdio = table
        .rows
        .iter()
        .find(|row| row.get("NAME") == "decl-researcher2-filesystem")
        .unwrap_or_else(|| panic!("no declared stdio server in\n{}", servers.stdout));
    assert_eq!(stdio.get("TRANSPORT"), "stdio");
    assert_eq!(stdio.get("COMMAND/URL"), "npx");
    assert_eq!(stdio.get("OWNER"), "declaration");

    let http = table
        .rows
        .iter()
        .find(|row| row.get("NAME") == "decl-researcher2-linear")
        .unwrap_or_else(|| panic!("no declared http server in\n{}", servers.stdout));
    assert_eq!(http.get("TRANSPORT"), "http");
    assert_eq!(http.get("OWNER"), "declaration");

    let remote = cx.run(["tools", "list"]).ok();
    assert!(
        remote
            .stdout
            .lines()
            .any(|line| line.contains("decl-researcher2-billing")
                && line.contains("https://internal.example.com")
                && line.contains("declaration")),
        "an OpenAPI service the agent brought is listed the same way:\n{}",
        remote.render()
    );
}

#[test]
fn deleting_the_declaration_retires_what_it_brought() {
    let cx = instance("decl-brings-retire");
    let path = cx
        .write_file(".contenox/agents/researcher2.md", BRINGS_TWO_SOURCES)
        .expect("write the declaration");
    discover(&cx).ok();
    assert!(cx.run(["mcp", "list"]).ok().stdout_has("decl-researcher2-"));

    std::fs::remove_file(&path).expect("delete the declaration");
    discover(&cx).ok();

    cx.run(["mcp", "list"])
        .ok()
        .refute_stdout("decl-researcher2-");
    cx.run(["tools", "list"])
        .ok()
        .refute_stdout("decl-researcher2-billing");
    assert_eq!(
        enabled(&cx, "researcher2"),
        Some(false),
        "and the agent itself stops being runnable"
    );
}

/// Anything the operator registered is never touched by a declaration; the
/// reverse holds too, so a hand-edited `declaration`-owned row is written back
/// from the file on the next pass.
#[test]
fn removing_a_declaration_owned_row_by_hand_does_not_stick() {
    let cx = instance("decl-brings-handedit");
    cx.write_file(".contenox/agents/researcher2.md", BRINGS_TWO_SOURCES)
        .expect("write the declaration");
    discover(&cx).ok();

    cx.run(["mcp", "remove", "decl-researcher2-filesystem"])
        .ok()
        .expect_stdout("removed");
    // The removal lands, so the edit is not refused at the wrist — it simply
    // does not survive. `contenox mcp list` runs no discovery pass of its own.
    cx.run(["mcp", "list"])
        .ok()
        .refute_stdout("decl-researcher2-filesystem");

    discover(&cx).ok();
    cx.run(["mcp", "list"])
        .ok()
        .expect_stdout("decl-researcher2-filesystem");
    assert!(
        cx.run(["mcp", "list"])
            .ok()
            .stdout
            .lines()
            .any(|line| line.contains("decl-researcher2-filesystem") && line.contains("npx")),
        "and it comes back from the file, command and all"
    );
}

#[test]
fn a_literal_credential_is_refused_naming_the_file_and_the_field() {
    let cx = instance("decl-literal-token");
    let path = cx
        .write_file(
            ".contenox/agents/leaky.md",
            "---\nname: leaky\ndescription: Carries a token in the file\nmcpServers:\n  \
             linear:\n    type: http\n    url: https://mcp.linear.app/mcp\n    \
             authToken: sk-live-abc123\n---\nBody.\n",
        )
        .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains(&format!("refused  {}", path.display())),
        "the refusal names the file:\n{}",
        out.render()
    );
    assert!(
        out.stderr.contains("mcpServers.linear sets \"authToken\""),
        "and the field:\n{}",
        out.render()
    );
    assert!(
        out.stderr
            .contains("Name an environment variable with authEnvKey instead"),
        "and what to write instead:\n{}",
        out.render()
    );
    assert!(!roster(&cx).contains(&"leaky".to_string()));
    cx.run(["mcp", "list"]).ok().refute_stdout("decl-leaky-");
}

#[test]
fn an_auth_env_key_is_accepted_where_a_literal_token_is_refused() {
    let cx = instance("decl-auth-env-key");
    cx.write_file(
        ".contenox/agents/tidy.md",
        "---\nname: tidy\ndescription: Names the variable, not its value\nmcpServers:\n  \
         linear:\n    type: http\n    url: https://mcp.linear.app/mcp\n    \
         authEnvKey: LINEAR_TOKEN\n---\nBody.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        !out.stderr.contains("refused"),
        "naming the variable is the supported shape:\n{}",
        out.render()
    );
    cx.run(["mcp", "list"])
        .ok()
        .expect_stdout("decl-tidy-linear");
}

// ---------------------------------------------------------------------------
// Posture: which envelope the agent runs under
// ---------------------------------------------------------------------------

#[test]
fn bypass_permissions_is_refused_and_says_what_to_write_instead() {
    let cx = instance("decl-bypass");
    let path = cx
        .write_file(
            ".contenox/agents/unbounded.md",
            "---\nname: unbounded\ndescription: Asks to skip every approval\ntools: Read\n\
             permissionMode: bypassPermissions\n---\nBody.\n",
        )
        .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains(&format!("refused  {}", path.display())),
        "{}",
        out.render()
    );
    assert!(
        out.stderr
            .contains("contenox will not run an agent that way"),
        "{}",
        out.render()
    );
    assert!(
        out.stderr
            .contains("under [policy.postures] or [[policy.always_allow]] in agents.toml"),
        "the refusal points at where a grant is written down instead:\n{}",
        out.render()
    );
    assert!(!roster(&cx).contains(&"unbounded".to_string()));
}

/// `posture` is contenox's own vocabulary and `permissionMode` is the imported
/// spelling. A declaration carrying both resolves to the posture, byte for
/// byte — and to something different from what the permissionMode alone says.
#[test]
fn posture_wins_over_permission_mode() {
    let cx = instance("decl-posture-wins");
    for (name, front) in [
        ("only-posture", "posture: read_only\n"),
        ("only-mode", "permissionMode: acceptEdits\n"),
        ("both", "posture: read_only\npermissionMode: acceptEdits\n"),
    ] {
        cx.write_file(
            &format!(".contenox/agents/{name}.md"),
            &format!("---\nname: {name}\ndescription: Posture probe\ntools: Read, Bash\n{front}---\nBody.\n"),
        )
        .expect("write the declaration");
    }
    discover(&cx).ok();

    let posture = emitted_policy(&cx, "only-posture");
    let mode = emitted_policy(&cx, "only-mode");
    let both = emitted_policy(&cx, "both");

    assert_eq!(
        both["rules"], posture["rules"],
        "both fields present must resolve exactly as the posture alone does"
    );
    assert_ne!(
        posture["rules"], mode["rules"],
        "and the two vocabularies must not already mean the same thing, \
         or the case above proves nothing"
    );
}

/// Each posture names a shipped envelope, and they are genuinely different
/// documents — a declaration cannot ask for a permission tier that resolves to
/// the same rules as every other.
#[test]
fn the_three_postures_resolve_to_three_different_policies() {
    let cx = instance("decl-three-postures");
    for posture in ["read_only", "ask_always", "auto_edit"] {
        cx.write_file(
            &format!(".contenox/agents/p-{posture}.md"),
            &format!(
                "---\nname: p-{posture}\ndescription: Posture probe\ntools: Read, Bash\n\
                 posture: {posture}\n---\nBody.\n"
            ),
        )
        .expect("write the declaration");
    }
    discover(&cx).ok();

    let rules: Vec<String> = ["read_only", "ask_always", "auto_edit"]
        .iter()
        .map(|p| emitted_policy(&cx, &format!("p-{p}"))["rules"].to_string())
        .collect();
    assert_ne!(rules[0], rules[1], "read_only and ask_always must differ");
    assert_ne!(rules[1], rules[2], "ask_always and auto_edit must differ");
    assert_ne!(rules[0], rules[2], "read_only and auto_edit must differ");
}

#[test]
fn an_unknown_posture_is_refused_and_lists_the_three_that_exist() {
    let cx = instance("decl-bad-posture");
    cx.write_file(
        ".contenox/agents/odd.md",
        "---\nname: odd\ndescription: Names a posture that does not exist\ntools: Read\n\
         posture: whatever\n---\nBody.\n",
    )
    .expect("write the declaration");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains("read_only")
            && out.stderr.contains("ask_always")
            && out.stderr.contains("auto_edit"),
        "the refusal names the vocabulary that does exist:\n{}",
        out.render()
    );
}

// ---------------------------------------------------------------------------
// Fields that are not carried
// ---------------------------------------------------------------------------

#[test]
fn memory_isolation_and_color_are_reported_as_not_carried() {
    let cx = instance("decl-not-carried");
    cx.write_file(
        ".contenox/agents/imported.md",
        "---\nname: imported\ndescription: Carries three fields contenox has no home for\n\
         tools: Read\nmemory: project\nisolation: container\ncolor: blue\n---\nBody.\n",
    )
    .expect("write the declaration");

    let stderr = discover(&cx).ok().stderr;
    for field in ["memory", "isolation", "color"] {
        assert!(
            stderr.contains(&format!("not carried  imported: {field} —")),
            "{field} must be reported, not dropped in silence:\n{stderr}"
        );
    }
    assert!(
        roster(&cx).contains(&"imported".to_string()),
        "and the agent still runs"
    );
}

#[test]
fn hooks_skills_and_background_are_reported_with_what_replaces_them() {
    let cx = instance("decl-replaced");
    cx.write_file(
        ".contenox/agents/ported.md",
        "---\nname: ported\ndescription: Carries three fields with a contenox counterpart\n\
         tools: Read\nhooks:\n  PreToolUse: echo hi\nskills: ./skills\nbackground: true\n---\n\
         Body.\n",
    )
    .expect("write the declaration");

    let stderr = discover(&cx).ok().stderr;
    assert!(
        stderr.contains("not carried  ported: hooks — contenox governs these in the runtime"),
        "hooks must say what replaces them:\n{stderr}"
    );
    assert!(
        stderr.contains("put them in .contenox/skills/ and write {{skills}} in the declaration"),
        "skills must point at the macro that replaces the field:\n{stderr}"
    );
    assert!(
        stderr.contains("not carried  ported: background — already the default"),
        "background must say it is already how a dispatched agent runs:\n{stderr}"
    );
}

// ---------------------------------------------------------------------------
// Branching: the directory is the chain
// ---------------------------------------------------------------------------

/// `agent.md` beside subdirectories is a router; each subdirectory is a branch
/// whose label is its own directory name, and the classifier prompt is told
/// which labels are valid so the two cannot drift.
#[test]
fn a_directory_of_declarations_is_one_agent_whose_agent_md_routes_to_its_subdirectories() {
    let cx = instance("decl-tree");
    cx.write_file(
        ".contenox/agents/sorter/agent.md",
        "---\nname: sorter\ndescription: Send a request to the branch that should handle it.\n\
         default: docs\n---\nYou sort an incoming request.\n",
    )
    .expect("write the router");
    cx.write_file(
        ".contenox/agents/sorter/code/agent.md",
        "---\nname: code\ndescription: Change code\n---\nYou change code.\n",
    )
    .expect("write the code branch");
    cx.write_file(
        ".contenox/agents/sorter/docs/agent.md",
        "---\nname: docs\ndescription: Change docs\n---\nYou change docs.\n",
    )
    .expect("write the docs branch");

    let names = roster(&cx);
    assert!(names.contains(&"sorter".to_string()), "{names:?}");
    assert!(
        !names.contains(&"code".to_string()) && !names.contains(&"docs".to_string()),
        "the tree is ONE agent, not one per leaf: {names:?}"
    );

    let chain = emitted_chain(&cx, "sorter");
    let route = task(&chain, "sorter-route");
    assert_eq!(route["handler"], "route");

    let prompt = route["system_instruction"].as_str().unwrap_or_default();
    assert!(
        prompt.contains("- code: Change code") && prompt.contains("- docs: Change docs"),
        "the classifier is told the labels and each branch's description:\n{prompt}"
    );
    assert!(
        prompt.contains("If none clearly applies, answer docs."),
        "and where an unmapped answer goes:\n{prompt}"
    );

    let branches = route["transition"]["branches"]
        .as_array()
        .expect("the router transitions");
    let goto = |label: &str| {
        branches
            .iter()
            .find(|b| b["when"] == label)
            .map(|b| b["goto"].as_str().unwrap_or_default().to_string())
    };
    assert_eq!(goto("code"), Some("sorter-code-agent".into()));
    assert_eq!(goto("docs"), Some("sorter-docs-agent".into()));
    assert_eq!(
        branches
            .iter()
            .find(|b| b["operator"] == "default")
            .map(|b| b["goto"].as_str().unwrap_or_default().to_string()),
        Some("sorter-docs-agent".into()),
        "an unsorted answer takes the declared default"
    );
    assert_eq!(
        route["transition"]["on_failure"], "sorter-docs-agent",
        "and so does a classifier that could not run at all"
    );
}

#[test]
fn a_default_naming_no_branch_is_refused() {
    let cx = instance("decl-tree-bad-default");
    cx.write_file(
        ".contenox/agents/baddef/agent.md",
        "---\nname: baddef\ndescription: Router whose default names no branch\n\
         default: nosuchbranch\n---\nSort it.\n",
    )
    .expect("write the router");
    for branch in ["one", "two"] {
        cx.write_file(
            &format!(".contenox/agents/baddef/{branch}/agent.md"),
            &format!("---\nname: {branch}\ndescription: Branch {branch}\n---\n{branch}.\n"),
        )
        .expect("write the branch");
    }

    let out = discover(&cx).ok();
    assert!(
        out.stderr
            .contains("declares default \"nosuchbranch\", which is not one of one, two"),
        "routing an unsorted request to whichever directory sorted first is the bug \
         this refusal exists to prevent:\n{}",
        out.render()
    );
    assert!(!roster(&cx).contains(&"baddef".to_string()));
}

#[test]
fn a_router_with_several_branches_and_no_default_is_refused() {
    let cx = instance("decl-tree-no-default");
    cx.write_file(
        ".contenox/agents/nodef/agent.md",
        "---\nname: nodef\ndescription: Router with no default\n---\nSort it.\n",
    )
    .expect("write the router");
    for branch in ["alpha", "beta"] {
        cx.write_file(
            &format!(".contenox/agents/nodef/{branch}/agent.md"),
            &format!("---\nname: {branch}\ndescription: Branch {branch}\n---\n{branch}.\n"),
        )
        .expect("write the branch");
    }

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains(
            "routes to alpha, beta and names no default — add `default: <one of them>` to its \
             agent.md"
        ),
        "the refusal says exactly what to add:\n{}",
        out.render()
    );
    assert!(!roster(&cx).contains(&"nodef".to_string()));
}

#[test]
fn a_router_with_exactly_one_branch_needs_no_default() {
    let cx = instance("decl-tree-one-branch");
    cx.write_file(
        ".contenox/agents/single/agent.md",
        "---\nname: single\ndescription: Router with one branch\n---\nSort it.\n",
    )
    .expect("write the router");
    cx.write_file(
        ".contenox/agents/single/only/agent.md",
        "---\nname: only\ndescription: The only branch\n---\nOnly.\n",
    )
    .expect("write the branch");

    assert!(roster(&cx).contains(&"single".to_string()));
    assert_eq!(
        task(&emitted_chain(&cx, "single"), "single-route")["transition"]["on_failure"],
        "single-only-agent",
        "with one branch there is nothing for a default to disambiguate"
    );
}

/// A recovery prompt is a different prompt, so it is a file. A branch that
/// should simply give up omits it, and an exhausted loop goes straight to the
/// failure report.
#[test]
fn a_branch_without_a_recovery_file_goes_straight_to_the_failure_report() {
    let cx = instance("decl-tree-recovery");
    cx.write_file(
        ".contenox/agents/pair/agent.md",
        "---\nname: pair\ndescription: Router\ndefault: patient\n---\nSort it.\n",
    )
    .expect("write the router");
    cx.write_file(
        ".contenox/agents/pair/patient/agent.md",
        "---\nname: patient\ndescription: Tries twice\n---\nPatient.\n",
    )
    .expect("write the patient branch");
    cx.write_file(
        ".contenox/agents/pair/patient/recovery.md",
        "---\nname: patient\ndescription: Second attempt\n---\nTry again.\n",
    )
    .expect("write the recovery prompt");
    cx.write_file(
        ".contenox/agents/pair/quitter/agent.md",
        "---\nname: quitter\ndescription: Gives up at once\n---\nQuitter.\n",
    )
    .expect("write the quitter branch");

    let chain = emitted_chain(&cx, "pair");
    let ids = task_ids(&chain);
    assert!(
        ids.contains(&"pair-patient-recovery".to_string()),
        "the branch with a recovery.md gets a second attempt: {ids:?}"
    );
    assert!(
        !ids.contains(&"pair-quitter-recovery".to_string()),
        "the branch without one gets none: {ids:?}"
    );
    assert_eq!(
        task(&chain, "pair-quitter-agent")["transition"]["on_failure"],
        "pair-summarise",
        "an exhausted loop with no recovery goes straight to the report"
    );
    assert_eq!(
        task(&chain, "pair-patient-agent")["transition"]["on_failure"],
        "pair-patient-recovery",
        "and one with a recovery prompt goes there first"
    );
}

#[test]
fn failure_md_is_what_the_tree_says_when_every_branch_has_given_up() {
    let cx = instance("decl-tree-failure");
    cx.write_file(
        ".contenox/agents/reporter/agent.md",
        "---\nname: reporter\ndescription: Router\n---\nSort it.\n",
    )
    .expect("write the router");
    cx.write_file(
        ".contenox/agents/reporter/only/agent.md",
        "---\nname: only\ndescription: The only branch\n---\nOnly.\n",
    )
    .expect("write the branch");
    cx.write_file(
        ".contenox/agents/reporter/failure.md",
        "---\nname: reporter\ndescription: The report\n---\n\
         Every branch gave up. Say what was attempted and stop.\n",
    )
    .expect("write the failure report");

    let chain = emitted_chain(&cx, "reporter");
    let terminal = task(&chain, "reporter-summarise");
    assert!(
        terminal["system_instruction"]
            .as_str()
            .unwrap_or_default()
            .starts_with("Every branch gave up."),
        "one report per tree, written where the tree can see it: {terminal:#}"
    );
}

// ---------------------------------------------------------------------------
// Telling the agent how much budget is left
// ---------------------------------------------------------------------------

/// A recovery prompt may not name a task or state a number. The round macros
/// become live edge counts over *this* leaf's own loop, so renaming the
/// directory cannot break the prompt, and the budgets become the numbers the
/// chain actually enforces — which is what the hand-written chains got wrong.
#[test]
fn the_round_macros_resolve_to_live_counts_and_to_the_budget_the_chain_enforces() {
    let cx = instance("decl-round-macros");
    cx.write_file(
        ".contenox/agents.toml",
        "[agents.budgeted.chain]\nmain_rounds = 5\nrecovery_rounds = 8\n",
    )
    .expect("write agents.toml");
    cx.write_file(
        ".contenox/agents/budgeted/agent.md",
        "---\nname: budgeted\ndescription: Router\n---\nSort it.\n",
    )
    .expect("write the router");
    cx.write_file(
        ".contenox/agents/budgeted/leaf/agent.md",
        "---\nname: leaf\ndescription: The only branch\n---\nWork.\n",
    )
    .expect("write the branch");
    cx.write_file(
        ".contenox/agents/budgeted/leaf/recovery.md",
        "---\nname: leaf\ndescription: Second attempt\n---\n\
         Used {{rounds_used}} of {{main_rounds}} main and \
         {{recovery_rounds_used}} of {{recovery_rounds}} recovery.\n",
    )
    .expect("write the recovery prompt");

    let chain = emitted_chain(&cx, "budgeted");
    let prompt = task(&chain, "budgeted-leaf-recovery")["system_instruction"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    assert!(
        prompt.starts_with(
            "Used {{edge_count:budgeted-leaf-agent->budgeted-leaf-tools}} of 5 main and \
             {{edge_count:budgeted-leaf-recovery->budgeted-leaf-recovery-tools}} of 8 recovery."
        ),
        "the counters are this leaf's own edges and the budgets are agents.toml's numbers:\n\
         {prompt}"
    );

    let main_cap = task(&chain, "budgeted-leaf-agent")["transition"]["branches"]
        .as_array()
        .expect("branches")
        .iter()
        .find(|b| b["operator"] == "edge_traversed_at_least")
        .map(|b| b["when"].as_str().unwrap_or_default().to_string());
    assert_eq!(
        main_cap,
        Some("5".into()),
        "and the number in the prompt is the number the transition enforces"
    );
    let recovery_cap = task(&chain, "budgeted-leaf-recovery")["transition"]["branches"]
        .as_array()
        .expect("branches")
        .iter()
        .find(|b| b["operator"] == "edge_traversed_at_least")
        .map(|b| b["when"].as_str().unwrap_or_default().to_string());
    assert_eq!(recovery_cap, Some("8".into()));
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

/// Only the one-line description costs context. The agent reads the file with
/// its ordinary file tool when a request matches.
#[test]
fn the_skills_macro_expands_to_the_one_line_index_not_the_bodies() {
    let cx = instance("decl-skills");
    cx.write_file(
        ".contenox/skills/timesheet.md",
        "---\nname: timesheet\ndescription: File this week's hours to the timesheet system\n---\n\
         Read the tracked hours, present the week for approval, submit the approved rows.\n",
    )
    .expect("write the skill");
    cx.write_file(
        ".contenox/agents/office.md",
        "---\nname: office\ndescription: Handles recurring office work\ntools: Read\n---\n\
         You handle recurring work.\n\n{{skills}}\n",
    )
    .expect("write the declaration");

    let prompt = task(&emitted_chain(&cx, "office"), "office-agent")["system_instruction"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    assert!(
        prompt.contains(
            "- timesheet: File this week's hours to the timesheet system — read \
             .contenox/skills/timesheet.md"
        ),
        "the index names the skill, its one line, and where to read it:\n{prompt}"
    );
    assert!(
        !prompt.contains("present the week for approval"),
        "the body must not be inlined — ten procedures cost ten lines:\n{prompt}"
    );
    assert!(
        !prompt.contains("{{skills}}"),
        "and the macro is expanded, not left for the model to interpret:\n{prompt}"
    );
}

#[test]
fn a_bare_markdown_skill_takes_its_name_from_the_file_and_its_line_from_the_first_line() {
    let cx = instance("decl-bare-skill");
    cx.write_file(
        ".contenox/skills/release.md",
        "Cut a release branch and tag it before the freeze.\n\n\
         The detail below must stay in the file.\n",
    )
    .expect("write the bare skill");
    cx.write_file(
        ".contenox/agents/office.md",
        "---\nname: office\ndescription: Handles recurring office work\ntools: Read\n---\n\
         You handle recurring work.\n\n{{skills}}\n",
    )
    .expect("write the declaration");

    let prompt = task(&emitted_chain(&cx, "office"), "office-agent")["system_instruction"]
        .as_str()
        .unwrap_or_default()
        .to_string();
    assert!(
        prompt.contains(
            "- release: Cut a release branch and tag it before the freeze. — read \
             .contenox/skills/release.md"
        ),
        "frontmatter is optional: the name comes from the filename and the line from the \
         first line:\n{prompt}"
    );
    assert!(
        !prompt.contains("The detail below must stay in the file."),
        "still the index, not the body:\n{prompt}"
    );
}

// ---------------------------------------------------------------------------
// What a declaration cannot say
// ---------------------------------------------------------------------------

/// A typo in a per-agent section is a knob that does nothing. Reporting it is
/// the difference between a setting that was applied and one that was not.
#[test]
fn an_agents_toml_section_naming_no_declaration_is_reported_not_ignored() {
    let cx = instance("decl-ghost-section");
    cx.write_file(
        ".contenox/agents.toml",
        "[agents.ghostagent.chain]\ntoken_limit = 32768\n",
    )
    .expect("write agents.toml");

    let out = discover(&cx).ok();
    assert!(
        out.stderr.contains(
            "ignored  agents.toml [agents.ghostagent]: no declaration by that name; the section \
             had no effect"
        ),
        "a section naming nothing must be reported:\n{}",
        out.render()
    );
}

#[test]
fn a_per_agent_section_reaches_the_agent_it_names() {
    let cx = instance("decl-per-agent-section");
    cx.write_file(
        ".contenox/agents/tuned.md",
        "---\nname: tuned\ndescription: Has its own budget\ntools: Read\n---\nBody.\n",
    )
    .expect("write the declaration");
    cx.write_file(
        ".contenox/agents/plain.md",
        "---\nname: plain\ndescription: Keeps the layer below\ntools: Read\n---\nBody.\n",
    )
    .expect("write the declaration");
    cx.write_file(
        ".contenox/agents.toml",
        "[agents.tuned.chain]\ntoken_limit = 32768\n",
    )
    .expect("write agents.toml");

    let out = discover(&cx).ok();
    assert!(
        !out.stderr.contains("ignored  agents.toml"),
        "a section naming a real declaration is not reported:\n{}",
        out.render()
    );
    assert_eq!(emitted_chain(&cx, "tuned")["token_limit"], 32768);
    assert_ne!(
        emitted_chain(&cx, "plain")["token_limit"],
        32768,
        "and it applies to that one agent, not to every agent"
    );
}

// ---------------------------------------------------------------------------
// The declaration is the source of truth
// ---------------------------------------------------------------------------

/// Registration by hand and registration by declaration coexist: the OWNER
/// column says which is which, and a discovery pass writes only its own rows.
#[test]
fn a_source_the_operator_registered_is_never_touched_by_a_declaration_pass() {
    let cx = instance("decl-owner-column");
    cx.run([
        "mcp",
        "add",
        "mine",
        "--transport",
        "http",
        "--url",
        "https://mine.example.com/mcp",
    ])
    .ok();
    cx.write_file(".contenox/agents/researcher2.md", BRINGS_TWO_SOURCES)
        .expect("write the declaration");
    discover(&cx).ok();

    let out = cx.run(["mcp", "list"]).ok();
    let table =
        contenox_e2e::Table::parse(&out.stdout, &["NAME", "TRANSPORT", "COMMAND/URL", "OWNER"])
            .expect("contenox mcp list prints its table");
    let owner = |name: &str| {
        table
            .rows
            .iter()
            .find(|row| row.get("NAME") == name)
            .map(|row| row.get("OWNER").to_string())
    };
    assert_eq!(owner("mine"), Some("you".into()));
    assert_eq!(owner("decl-researcher2-linear"), Some("declaration".into()));

    // And the operator's row outlives the declaration that ran beside it.
    std::fs::remove_file(cx.work().join(".contenox/agents/researcher2.md"))
        .expect("delete the declaration");
    discover(&cx).ok();
    cx.run(["mcp", "list"])
        .ok()
        .expect_stdout("mine")
        .refute_stdout("decl-researcher2-");
}

/// `contenox doctor` is what the guide sends you to for the complete roster,
/// so a source an agent brought has to be in it.
#[test]
fn a_source_a_declaration_brought_appears_in_the_roster_doctor_prints() {
    let cx = instance("decl-doctor-roster");
    cx.write_file(".contenox/agents/researcher2.md", BRINGS_TWO_SOURCES)
        .expect("write the declaration");
    discover(&cx).ok();

    cx.doctor()
        .ok()
        .expect_stdout("decl-researcher2-filesystem — MCP server (stdio npx)")
        .expect_stdout("decl-researcher2-linear — MCP server (http https://mcp.linear.app/mcp)");
}

/// Disabling is an operator decision about a declaration that still exists, so
/// it survives the pass that re-reads the file.
#[test]
fn disabling_a_declared_agent_sticks_across_the_next_discovery_pass() {
    let cx = instance("decl-disable");
    cx.write_file(
        ".contenox/agents/switchable.md",
        "---\nname: switchable\ndescription: Can be turned off\ntools: Read\n---\nBody.\n",
    )
    .expect("write the declaration");
    assert_eq!(enabled(&cx, "switchable"), Some(true));

    cx.run(["agent", "disable", "switchable"])
        .ok()
        .expect_stdout("Agent \"switchable\" disabled.");
    assert_eq!(
        enabled(&cx, "switchable"),
        Some(false),
        "and the discovery pass agent list runs must not turn it back on"
    );

    cx.run(["agent", "enable", "switchable"])
        .ok()
        .expect_stdout("Agent \"switchable\" enabled.");
    assert_eq!(enabled(&cx, "switchable"), Some(true));
}

/// `remove` deletes the local registration only. The file is the source of
/// truth, so the next discovering command registers it again — retiring an
/// agent means deleting its declaration.
#[test]
fn removing_a_declared_agent_does_not_outlive_its_declaration_file() {
    let cx = instance("decl-remove");
    let path = cx
        .write_file(
            ".contenox/agents/stubborn.md",
            "---\nname: stubborn\ndescription: Comes back\ntools: Read\n---\nBody.\n",
        )
        .expect("write the declaration");
    assert!(roster(&cx).contains(&"stubborn".to_string()));

    cx.run(["agent", "remove", "stubborn"])
        .ok()
        .expect_stdout("Agent \"stubborn\" removed.");
    assert!(
        roster(&cx).contains(&"stubborn".to_string()),
        "the declaration is still on disk, so the next pass registers it again"
    );

    std::fs::remove_file(&path).expect("delete the declaration");
    assert_eq!(
        enabled(&cx, "stubborn"),
        Some(false),
        "deleting the file is what retires the agent"
    );
}
