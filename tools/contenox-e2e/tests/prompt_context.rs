//! What an agent starts a session knowing: the project's `AGENTS.md`, and the
//! skill index a declaration pulls in with `{{skills}}`.
//!
//! Both are read back the way the documentation tells an operator to read them
//! — `contenox session show --head 1` for the project context, the chain
//! `contenox agent show` points at for the expanded index — and, where the
//! claim is about what the *model* was told rather than what a file says,
//! through `contenox state show --raw`, which records the prompt with its
//! macros already expanded.

use contenox_e2e::{Acp, Instance, Script, ToolCall, Turn};
use serde_json::Value;
use std::path::Path;
use std::time::Duration;

/// Anything that builds an engine and holds a turn.
const SLOW: Duration = Duration::from_secs(180);

/// A scratch instance whose model answers the same line whenever it is asked.
/// Each command is its own process and the scripted dialog replays from the
/// top, so one turn serves any number of invocations.
fn instance(label: &str) -> Instance {
    let cx = Instance::named(label).expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().turn("Noted."))
        .expect("scripted-test backend");
    cx
}

/// Hold one turn of conversation, from `dir`, in whichever session is active.
fn chat_in(cx: &Instance, dir: &Path, message: &str) {
    cx.cmd(["chat", message])
        .cwd(dir)
        .timeout(SLOW)
        .output()
        .expect("contenox chat")
        .ok();
}

fn chat(cx: &Instance, message: &str) {
    chat_in(cx, cx.work(), message);
}

/// The documented verification recipe, run from `dir`: the head of the active
/// session.
fn head_from(cx: &Instance, dir: &Path) -> String {
    cx.cmd(["session", "show", "--head", "1"])
        .cwd(dir)
        .output()
        .expect("contenox session show --head 1")
        .ok()
        .stdout
        .clone()
}

fn head(cx: &Instance) -> String {
    head_from(cx, cx.work())
}

/// The whole active conversation, for a case counting what is in it.
fn transcript(cx: &Instance) -> String {
    cx.run(["session", "show"]).ok().stdout.clone()
}

/// The role of the first message `session show` printed.
fn first_role(shown: &str) -> String {
    for line in shown.lines() {
        let line = line.trim_end();
        if line.is_empty() || line.starts_with('━') || line.starts_with(' ') {
            continue;
        }
        let label = line.rsplit("] ").next().unwrap_or(line);
        if let Some(role) = label.strip_suffix(':') {
            return role.to_string();
        }
    }
    String::new()
}

fn write_agents_md(cx: &Instance, dir: &str, body: &str) -> String {
    let relative = if dir.is_empty() {
        "AGENTS.md".to_string()
    } else {
        format!("{dir}/AGENTS.md")
    };
    cx.write_file(&relative, body)
        .expect("write AGENTS.md")
        .display()
        .to_string()
}

/// The system prompt of the agent task in the chain `contenox agent show`
/// points at — the artefact the macro expansion is written into.
fn generated_prompt(cx: &Instance, agent: &str) -> String {
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
    let chain: Value = serde_json::from_str(&text).expect("the emitted chain is JSON");
    chain["tasks"]
        .as_array()
        .expect("a chain has tasks")
        .iter()
        .find(|task| task["id"] == format!("{agent}-agent"))
        .and_then(|task| task["system_instruction"].as_str())
        .unwrap_or_else(|| panic!("no {agent}-agent prompt in {text}"))
        .to_string()
}

/// A declaration that pulls the inventory in, and one procedure to find.
fn office_with_skills(cx: &Instance) {
    cx.write_file(
        ".contenox/skills/timesheet.md",
        "---\nname: timesheet\ndescription: File this week's hours to the timesheet system\n---\n\
         Read the tracked hours from the time tool, present the week for approval.\n",
    )
    .expect("write the skill");
    cx.write_file(
        ".contenox/agents/office.md",
        "---\nname: office\ndescription: Handles recurring office work\ntools: Read\n---\n\
         You handle recurring work.\n\n{{skills}}\n",
    )
    .expect("write the declaration");
}

// =============================================================== AGENTS.md

/// The recipe the documentation gives for confirming it loaded, run verbatim.
#[test]
fn agents_md_at_the_project_root_is_the_first_message_of_a_new_session() {
    let cx = instance("agentsmd-root");
    let at = write_agents_md(
        &cx,
        "",
        "# Probe project\n\n## Don't do\n- ROOT-MARKER: never run git push --force\n",
    );

    chat(&cx, "hello");
    let shown = head(&cx);

    assert_eq!(
        first_role(&shown),
        "system",
        "the head of a new session is a system message:\n{shown}"
    );
    assert!(
        shown.contains(&format!("Project context loaded from {at}")),
        "the wrapper names the file it came from:\n{shown}"
    );
    assert!(
        shown.contains("ROOT-MARKER: never run git push --force"),
        "and carries the file's own content:\n{shown}"
    );
}

/// The recipe only verifies anything if it reads differently when no file is
/// there — otherwise every project looks loaded.
#[test]
fn a_project_with_no_agents_md_starts_the_session_with_the_users_own_message() {
    let cx = instance("agentsmd-absent");

    chat(&cx, "hello with no project context");
    let shown = head(&cx);

    assert_eq!(
        first_role(&shown),
        "user",
        "with no AGENTS.md the conversation opens on the operator:\n{shown}"
    );
    assert!(
        !shown.contains("Project context loaded from"),
        "nothing may be claimed to have loaded:\n{shown}"
    );
}

/// "at the project root or any parent" — the loader walks up, so a session
/// started deep in the tree still gets the root file.
#[test]
fn the_loader_walks_up_from_the_working_directory_to_a_parent() {
    let cx = instance("agentsmd-walk-up");
    let at = write_agents_md(&cx, "", "# Monorepo root\n- ROOT-MARKER\n");
    let deep = cx.work().join("services").join("api").join("internal");
    std::fs::create_dir_all(&deep).expect("create the nested package");

    chat_in(&cx, &deep, "hello from deep in the tree");
    let shown = head_from(&cx, &deep);

    assert!(
        shown.contains(&format!("Project context loaded from {at}")),
        "a directory with no AGENTS.md of its own inherits the one above it:\n{shown}"
    );
}

/// Nested files are a monorepo shipping per-package context: the walk stops at
/// the first hit, so the nearer file is the one that loads.
#[test]
fn the_closest_agents_md_wins_and_the_walk_stops_there() {
    let cx = instance("agentsmd-closest");
    write_agents_md(&cx, "", "# Monorepo root\n- ROOT-MARKER\n");
    let inner = write_agents_md(&cx, "pkg/inner", "# Inner package\n- INNER-MARKER\n");
    let at = cx.work().join("pkg").join("inner");

    chat_in(&cx, &at, "hello from the package");
    let shown = head_from(&cx, &at);

    assert!(
        shown.contains(&format!("Project context loaded from {inner}")),
        "the closest AGENTS.md is the one loaded:\n{shown}"
    );
    assert!(
        shown.contains("INNER-MARKER"),
        "and its content is what arrived:\n{shown}"
    );
    assert!(
        !shown.contains("ROOT-MARKER"),
        "the walk stops at the first hit; the root file is not appended:\n{shown}"
    );
}

/// The cap is a promise about what reaches the model, and the marker is how a
/// reader of the transcript knows the tail is missing.
#[test]
fn an_agents_md_over_64_kib_is_truncated_with_a_marker() {
    let cx = instance("agentsmd-cap");
    let mut body = String::from("# Big\n- HEAD-MARKER\n");
    while body.len() < 96 * 1024 {
        body.push_str("PADDING-LINE-0123456789 0123456789 0123456789\n");
    }
    body.push_str("- TAIL-MARKER\n");
    assert!(body.len() > 64 * 1024, "the fixture must exceed the cap");
    write_agents_md(&cx, "", &body);

    chat(&cx, "hello");
    let shown = head(&cx);

    assert!(
        shown.contains("- HEAD-MARKER"),
        "the head of the file survives:\n{}",
        &shown[..shown.len().min(400)]
    );
    assert!(
        !shown.contains("- TAIL-MARKER"),
        "everything past 64 KiB is dropped"
    );
    assert!(
        shown.contains("[AGENTS.md truncated to 64 KiB"),
        "and the transcript says so, rather than ending mid-sentence in silence"
    );
}

/// Read at session start and persisted: an edit lands in the file, not in the
/// conversation that already loaded it.
#[test]
fn the_copy_loaded_at_session_start_does_not_follow_a_later_edit() {
    let cx = instance("agentsmd-stale");
    write_agents_md(&cx, "", "# Probe\n- FIRST-CONTENT\n");

    chat(&cx, "first");
    write_agents_md(&cx, "", "# Probe\n- SECOND-CONTENT\n");
    chat(&cx, "second");

    let shown = transcript(&cx);
    assert!(
        shown.contains("FIRST-CONTENT"),
        "the copy loaded at session start is still the one in history:\n{shown}"
    );
    assert!(
        !shown.contains("SECOND-CONTENT"),
        "the edit does not reach a session that already started:\n{shown}"
    );
}

/// Once per session, not once per turn: a long conversation must not pay for
/// the file again on every message.
#[test]
fn the_project_context_is_loaded_once_per_session_not_once_per_turn() {
    let cx = instance("agentsmd-once");
    write_agents_md(&cx, "", "# Probe\n- ONLY-ONCE\n");

    chat(&cx, "first");
    chat(&cx, "second");
    chat(&cx, "third");

    let shown = transcript(&cx);
    assert_eq!(
        shown.matches("Project context loaded from").count(),
        1,
        "three turns, one system message:\n{shown}"
    );
}

/// "every new session" — the second session in the same project gets its own
/// copy, not the first session's.
#[test]
fn a_second_session_in_the_same_project_loads_it_again() {
    let cx = instance("agentsmd-second-session");
    write_agents_md(&cx, "", "# Probe\n- BOTH-SESSIONS\n");

    chat(&cx, "first session");
    cx.run(["session", "new", "second"]).ok();
    chat(&cx, "second session");

    let shown = head(&cx);
    assert_eq!(
        first_role(&shown),
        "system",
        "the fresh session opens on the project context too:\n{shown}"
    );
    assert!(
        shown.contains("BOTH-SESSIONS"),
        "with the same file behind it:\n{shown}"
    );
}

/// The transcript is evidence only if it is the same text the model received.
#[test]
fn the_project_context_the_session_shows_is_what_the_model_was_sent() {
    let cx = instance("agentsmd-reached-model");
    write_agents_md(&cx, "", "# Probe\n- SENT-TO-THE-MODEL\n");

    chat(&cx, "hello");

    let sent = cx
        .captured_system_prompts()
        .expect("contenox state show --raw");
    assert!(
        sent.iter()
            .any(|prompt| prompt.contains("SENT-TO-THE-MODEL")),
        "the loaded file must be in the messages the model was sent, got {sent:?}"
    );
}

/// The documentation names the editor surfaces first: "an ACP session
/// (`contenox acp` / `acpx`) or one created with `contenox session new`".
#[test]
#[ignore = "confirmed defect: an ACP session never loads AGENTS.md, so an editor — and beam, which is the same session machinery — starts with none of the project's context, while `contenox chat` in the same directory loads it. Seam: only chat_cmd.go calls loadAgentsMDFromCwd() and fills PromptRequest.AgentsMD; acpsvc/prompt.go and acpsvc/native_turn.go build agentservice.PromptRequest without AgentsMD/AgentsMDSource, and req.Cwd — which is where the walk would have to start for an editor launched anywhere — is never consulted. docs/guide/agents-md.md promises the opposite."]
fn an_editor_session_loads_the_projects_agents_md_too() {
    let cx = Instance::named("agentsmd-editor").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("Noted."))
        .expect("scripted-test backend");
    write_agents_md(&cx, "", "# Probe\n- EDITOR-MARKER\n");

    let mut acp = cx.acp(["acp"]).expect("spawn the editor surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "hello").expect("session/prompt");
    acp.close()
        .expect("the surface exits when its client hangs up");

    let sent = cx
        .captured_system_prompts()
        .expect("contenox state show --raw");
    assert!(
        sent.iter().any(|prompt| prompt.contains("EDITOR-MARKER")),
        "an editor session must start with the project's AGENTS.md, got {sent:?}"
    );
}

// ================================================================== skills

/// Generation time, not request time: the index is baked into the artefact,
/// while the macros that genuinely vary per request are left for the engine.
#[test]
fn the_skills_macro_is_expanded_into_the_generated_chain_and_the_per_request_macros_are_not() {
    let cx = instance("skills-generated");
    office_with_skills(&cx);

    let prompt = generated_prompt(&cx, "office");

    assert!(
        prompt.contains("- timesheet: File this week's hours to the timesheet system — read"),
        "the index is already in the file the chain was generated into:\n{prompt}"
    );
    assert!(
        !prompt.contains("{{skills}}"),
        "nothing is left for the engine to expand per request:\n{prompt}"
    );
    assert!(
        prompt.contains("{{tools}}"),
        "while a macro that does vary per request is still a macro here:\n{prompt}"
    );
}

/// The stable prefix the expansion buys: every model call in a session is sent
/// the same one, so a provider can cache it.
#[test]
fn the_expanded_index_is_the_same_prefix_on_every_call_of_a_session() {
    let cx = Instance::named("skills-stable-prefix").expect("scratch instance");
    cx.init().ok();
    office_with_skills(&cx);
    let chain = cx.generated("chain-agent-office.json");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Reading the procedure first.")
                    .call(ToolCall::new("read_file").arg("path", ".contenox/skills/timesheet.md")),
            )
            .turn("Filed the hours."),
    )
    .expect("scripted-test backend");
    cx.run(["agent", "list"]).ok();

    let mut acp = Acp::spawn(cx.cmd(["acpx"]).env("CONTENOX_ACPX_CHAIN_PATH", &chain))
        .expect("spawn the headless ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "file this week's hours")
        .expect("session/prompt");
    acp.close()
        .expect("the surface exits when its client hangs up");

    let calls = cx
        .executed_tasks()
        .expect("contenox state show")
        .iter()
        .filter(|step| step.handler == "chat_completion")
        .count();
    assert_eq!(
        calls, 2,
        "the turn has to reach the model twice to prove anything"
    );

    let sent = cx
        .captured_system_prompts()
        .expect("contenox state show --raw");
    let carrying: Vec<&String> = sent
        .iter()
        .filter(|prompt| prompt.contains("Skills are procedures for repeated work."))
        .collect();
    assert_eq!(
        carrying.len(),
        1,
        "two model calls, one prompt prefix between them, got {carrying:#?}"
    );
}

/// The index is an instruction the agent must be able to act on, so it lists
/// only what the agent's file tool — rooted at the project — can open.
#[test]
fn a_skill_in_the_operator_home_directory_is_not_listed_beside_the_workspaces_own() {
    let cx = instance("skills-home-excluded");
    office_with_skills(&cx);
    cx.write_home_file(
        "skills/homeonly.md",
        "---\nname: homeonly\ndescription: A procedure kept in the operator home directory\n---\n\
         Body.\n",
    )
    .expect("write the home skill");

    let prompt = generated_prompt(&cx, "office");

    assert!(
        prompt.contains("- timesheet:"),
        "the workspace's own skill is listed:\n{prompt}"
    );
    assert!(
        !prompt.contains("homeonly"),
        "one under ~/.contenox/skills is not: the agent could not open it:\n{prompt}"
    );
}

/// The index costs one line; the body costs a tool call. The path it prints has
/// to be one that call can actually open.
#[test]
fn the_agent_reads_the_skill_itself_with_the_ordinary_file_tool() {
    let cx = Instance::named("skills-read-through-tool").expect("scratch instance");
    cx.init().ok();
    office_with_skills(&cx);
    let chain = cx.generated("chain-agent-office.json");
    cx.scripted(
        &Script::new()
            .turn(
                Turn::new()
                    .text("Reading the procedure first.")
                    .call(ToolCall::new("read_file").arg("path", ".contenox/skills/timesheet.md")),
            )
            .turn("Filed the hours."),
    )
    .expect("scripted-test backend");

    let index = generated_prompt(&cx, "office");
    assert!(
        index.contains("read .contenox/skills/timesheet.md"),
        "the index prints the path to read:\n{index}"
    );

    let mut acp = Acp::spawn(cx.cmd(["acpx"]).env("CONTENOX_ACPX_CHAIN_PATH", &chain))
        .expect("spawn the headless ACP surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    let turn = acp
        .prompt(&session, "file this week's hours")
        .expect("session/prompt");
    acp.close()
        .expect("the surface exits when its client hangs up");

    let asked_for = cx
        .work()
        .join(".contenox/skills/timesheet.md")
        .display()
        .to_string();
    assert!(
        turn.read_paths().contains(&asked_for),
        "the skill is fetched by the same file tool every other read goes through, got {:?}",
        turn.read_paths()
    );
    assert!(
        turn.tool_outputs()
            .contains("Read the tracked hours from the time tool"),
        "and the body reaches the model only then:\n{}",
        turn.tool_outputs()
    );
    assert!(
        !turn.asked_permission(),
        "a read is not gated, here no more than anywhere else"
    );
}

/// Beam is the front door, and the same session machinery underneath. The
/// operator who wrote an AGENTS.md and opened beam in that project has every
/// reason to expect the agent to have read it.
#[test]
#[ignore = "confirmed defect: beam loads no AGENTS.md either — same seam as the editor case above (acpsvc builds the PromptRequest without it), so the project context an operator wrote is absent from the shape the product opens by default. `contenox chat` in the same directory loads it."]
fn beam_starts_its_session_with_the_projects_agents_md() {
    let cx = Instance::named("agentsmd-beam").expect("scratch instance");
    cx.init().ok();
    cx.scripted(&Script::new().route("general").turn("Noted."))
        .expect("scripted-test backend");
    write_agents_md(&cx, "", "# Probe\n- BEAM-MARKER\n");

    let mut pty = cx.pty(["beam", "--plain"]).expect("beam under a pty");
    pty.wait_for("type / for commands", Duration::from_secs(90))
        .expect("beam's composer");
    pty.send_line("hello").expect("submit the prompt");
    pty.wait_for("Noted.", Duration::from_secs(120))
        .expect("the answer");
    pty.interrupt();

    let sessions = cx.sessions_all().expect("contenox session list --all");
    let [only] = sessions.as_slice() else {
        panic!("expected exactly one session, got {sessions:?}");
    };
    let shown = cx
        .run(["session", "show", &only.id, "--head", "1"])
        .ok()
        .stdout
        .clone();
    assert_eq!(
        first_role(&shown),
        "system",
        "beam's session opens on the project context too:\n{shown}"
    );
    assert!(
        shown.contains("BEAM-MARKER"),
        "with the file the operator wrote behind it:\n{shown}"
    );
}

/// Every path in the index, as the model was given it.
fn indexed_paths(prompt: &str) -> Vec<String> {
    prompt
        .lines()
        .filter(|line| line.starts_with("- "))
        .filter_map(|line| line.rsplit_once(" — read "))
        .map(|(_, path)| path.trim().to_string())
        .collect()
}

/// The index is only worth its line if the session it is served to can act on
/// it: the paths are read relative to the project the session has open.
#[test]
#[ignore = "confirmed defect: an editor session is served the skills of ~/.contenox/skills/ and none of the project's own. The ACP surfaces compile their chain from the home directory (runACPProfile uses globalContenoxDir), and syncDeclaredAgents hands DiscoverSkills that directory as both the contenox dir and — via workspaceRootsForSync — the workspace root, so a home skill passes the readablePath check and is listed as `.contenox/skills/<name>.md` relative to $HOME. The agent's file tool is rooted at the session's cwd, so that read cannot resolve, and the project's own .contenox/skills/ is never scanned. docs/guide/agents.md promises the opposite, and gives this exact failure as the reason for the rule: \"an entry it cannot open would be an instruction that fails\"."]
fn an_editor_session_is_served_the_projects_skills_and_can_open_every_one() {
    let cx = Instance::named("skills-editor-index").expect("scratch instance");
    cx.init().ok();
    office_with_skills(&cx);
    cx.write_home_file(
        "skills/homeonly.md",
        "---\nname: homeonly\ndescription: A procedure kept in the operator home directory\n---\n\
         Body.\n",
    )
    .expect("write the home skill");

    // The operator pulls the inventory into the agent their editor talks to,
    // which is the copy of the shipped declaration in their own directory.
    let declaration = cx.home_file("agents/acpx.md");
    let mut body = std::fs::read_to_string(&declaration).expect("the seeded acpx declaration");
    body.push_str("\n{{skills}}\n");
    std::fs::write(&declaration, body).expect("declare the inventory");

    cx.scripted(&Script::new().turn("Reporting in."))
        .expect("scripted-test backend");

    // A surface that carries the fleet compiles the edited declaration on the
    // way up; this one holds no conversation, so the run read back below is the
    // editor session that follows it.
    let mut fleet = cx.acp(["acp"]).expect("spawn the fleet-carrying surface");
    fleet.initialize().expect("initialize");
    fleet.new_session(cx.work()).expect("session/new");
    fleet
        .close()
        .expect("the surface exits when its client hangs up");

    let mut acp = cx.acp(["acpx"]).expect("spawn the editor surface");
    acp.initialize().expect("initialize");
    let session = acp.new_session(cx.work()).expect("session/new");
    acp.prompt(&session, "what can you do?")
        .expect("session/prompt");
    acp.close()
        .expect("the surface exits when its client hangs up");

    let sent = cx
        .captured_system_prompts()
        .expect("contenox state show --raw");
    let index = sent
        .iter()
        .find(|prompt| prompt.contains("Skills are procedures for repeated work."))
        .unwrap_or_else(|| panic!("the editor's agent was served no index at all, got {sent:?}"));

    let listed = indexed_paths(index);
    for path in &listed {
        assert!(
            cx.work().join(path).is_file(),
            "the index tells the agent to read {path}, which does not exist in the project the \
             session has open; every entry must be one its file tool can open. Listed: {listed:?}"
        );
    }
    assert!(
        listed.iter().any(|path| path.ends_with("timesheet.md")),
        "and the project's own procedure is the one this session can use, got {listed:?}"
    );
}
