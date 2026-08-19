use crate::cmd::CmdOutput;
use crate::instance::Instance;
use crate::table::Table;
use anyhow::{Context, Result, bail};
use std::collections::BTreeMap;
use std::path::Path;
use std::time::{Duration, Instant};

#[derive(Debug, Clone)]
pub struct MissionRow {
    pub id: String,
    pub agent: String,
    pub envelope: String,
    pub status: String,
    pub age: String,
}

#[derive(Debug, Clone)]
pub struct MissionShow {
    pub fields: BTreeMap<String, String>,
    pub body: String,
}

impl MissionShow {
    pub fn get(&self, field: &str) -> &str {
        self.fields
            .get(field)
            .map(String::as_str)
            .unwrap_or_default()
    }
}

#[derive(Debug, Clone)]
pub struct ApprovalRow {
    pub id: String,
    pub kind: String,
    pub tool: String,
    pub summary: String,
    pub mission: String,
    pub age: String,
    pub expires_in: String,
}

#[derive(Debug, Clone)]
pub struct SessionRow {
    pub name: String,
    pub messages: u32,
    pub current: bool,
}

#[derive(Debug, Clone)]
pub struct WorkspaceSessionRow {
    pub workspace: String,
    pub name: String,
    pub identity: String,
    pub messages: String,
    pub id: String,
}

#[derive(Debug, Clone)]
pub struct BackendRow {
    pub name: String,
    pub kind: String,
    pub url: String,
}

/// One step of a captured chain run, as `contenox state show` prints it.
#[derive(Debug, Clone)]
pub struct StateStep {
    pub task: String,
    pub handler: String,
    pub transition: String,
    pub status: String,
}

impl Instance {
    pub fn missions(&self) -> Result<Vec<MissionRow>> {
        let out = self.run(["mission", "list"]);
        if !out.success() {
            bail!("contenox mission list failed\n{}", out.render());
        }
        let table = table_or_empty(&out.stdout, &["ID", "AGENT", "ENVELOPE", "STATUS", "AGE"])?;
        Ok(table
            .rows
            .iter()
            .map(|row| MissionRow {
                id: row.get("ID").to_string(),
                agent: row.get("AGENT").to_string(),
                envelope: row.get("ENVELOPE").to_string(),
                status: row.get("STATUS").to_string(),
                age: row.get("AGE").to_string(),
            })
            .collect())
    }

    pub fn mission_show(&self, id: &str) -> Result<MissionShow> {
        let out = self.run(["mission", "show", id]);
        if !out.success() {
            bail!("contenox mission show {id} failed\n{}", out.render());
        }
        Ok(MissionShow {
            fields: labelled_fields(&out.stdout),
            body: out.stdout.clone(),
        })
    }

    pub fn mission_reports(&self, id: &str) -> CmdOutput {
        self.run(["mission", "reports", id])
    }

    pub fn approvals(&self) -> Result<Vec<ApprovalRow>> {
        let out = self.run(["approvals", "list"]);
        if !out.success() {
            bail!("contenox approvals list failed\n{}", out.render());
        }
        let table = table_or_empty(
            &out.stdout,
            &[
                "ID",
                "KIND",
                "TOOL",
                "SUMMARY",
                "MISSION",
                "AGE",
                "EXPIRES-IN",
            ],
        )?;
        Ok(table
            .rows
            .iter()
            .map(|row| ApprovalRow {
                id: row.get("ID").to_string(),
                kind: row.get("KIND").to_string(),
                tool: row.get("TOOL").to_string(),
                summary: row.get("SUMMARY").to_string(),
                mission: row.get("MISSION").to_string(),
                age: row.get("AGE").to_string(),
                expires_in: row.get("EXPIRES-IN").to_string(),
            })
            .collect())
    }

    pub fn await_approval(&self, timeout: Duration) -> Result<ApprovalRow> {
        let deadline = Instant::now() + timeout;
        loop {
            let pending = self.approvals()?;
            if let Some(row) = pending.into_iter().next() {
                return Ok(row);
            }
            if Instant::now() >= deadline {
                bail!("no ask reached 'contenox approvals list' within {timeout:?}");
            }
            std::thread::sleep(Duration::from_millis(250));
        }
    }

    pub fn approve(&self, id: &str) -> CmdOutput {
        self.run(["approvals", "respond", id, "--approve"])
    }

    pub fn deny(&self, id: &str) -> CmdOutput {
        self.run(["approvals", "respond", id, "--deny"])
    }

    pub fn answer(&self, id: &str, text: &str) -> CmdOutput {
        self.run(["approvals", "respond", id, "--answer", text])
    }

    pub fn sessions(&self) -> Result<Vec<SessionRow>> {
        self.sessions_in(self.work())
    }

    /// `contenox session list` run from `dir`, which is what fixes the
    /// workspace the listing is scoped to.
    pub fn sessions_in(&self, dir: impl AsRef<Path>) -> Result<Vec<SessionRow>> {
        let out = self
            .cmd(["session", "list"])
            .cwd(dir.as_ref())
            .output()
            .with_context(|| format!("contenox session list in {}", dir.as_ref().display()))?;
        if !out.success() {
            bail!("contenox session list failed\n{}", out.render());
        }
        if out.stdout.contains("No sessions yet.") {
            return Ok(Vec::new());
        }
        Ok(out.stdout.lines().filter_map(parse_session_line).collect())
    }

    pub fn sessions_all(&self) -> Result<Vec<WorkspaceSessionRow>> {
        let out = self.run(["session", "list", "--all"]);
        if !out.success() {
            bail!("contenox session list --all failed\n{}", out.render());
        }
        let table = table_or_empty(
            &out.stdout,
            &["WORKSPACE", "NAME", "IDENTITY", "MESSAGES", "ID"],
        )?;
        Ok(table
            .rows
            .iter()
            .map(|row| WorkspaceSessionRow {
                workspace: row.get("WORKSPACE").to_string(),
                name: row.get("NAME").to_string(),
                identity: row.get("IDENTITY").to_string(),
                messages: row.get("MESSAGES").to_string(),
                id: row.get("ID").to_string(),
            })
            .collect())
    }

    pub fn session_show(&self, name: &str) -> CmdOutput {
        self.run(["session", "show", name])
    }

    pub fn backends(&self) -> Result<Vec<BackendRow>> {
        let out = self.run(["backend", "list"]);
        if !out.success() {
            bail!("contenox backend list failed\n{}", out.render());
        }
        let table = table_or_empty(&out.stdout, &["NAME", "TYPE", "URL"])?;
        Ok(table
            .rows
            .iter()
            .map(|row| BackendRow {
                name: row.get("NAME").to_string(),
                kind: row.get("TYPE").to_string(),
                url: row.get("URL").to_string(),
            })
            .collect())
    }

    pub fn state_requests(&self) -> Result<Vec<String>> {
        let out = self.run(["state", "list"]);
        if !out.success() {
            bail!("contenox state list failed\n{}", out.render());
        }
        if out.stdout.contains("(no captured state)") {
            return Ok(Vec::new());
        }
        Ok(out
            .stdout
            .lines()
            .map(str::trim)
            .filter(|line| !line.is_empty())
            .map(str::to_string)
            .collect())
    }

    pub fn state_steps(&self, request: &str) -> Result<Vec<StateStep>> {
        let out = self.run(["state", "show", request]);
        if !out.success() {
            bail!("contenox state show {request} failed\n{}", out.render());
        }
        let table = Table::parse(
            &out.stdout,
            &[
                "TASK",
                "HANDLER",
                "RETRY",
                "DURATION",
                "TRANSITION",
                "STATUS",
            ],
        )?;
        Ok(table
            .rows
            .iter()
            .map(|row| StateStep {
                task: row.get("TASK").to_string(),
                handler: row.get("HANDLER").to_string(),
                transition: row.get("TRANSITION").to_string(),
                status: row.get("STATUS").to_string(),
            })
            .collect())
    }

    /// The steps of the one captured run, for a case that made exactly one.
    pub fn executed_tasks(&self) -> Result<Vec<StateStep>> {
        let requests = self.state_requests()?;
        match requests.as_slice() {
            [only] => self.state_steps(only),
            [] => bail!("contenox state list recorded nothing to read back"),
            many => bail!("expected one captured run, got {many:?}"),
        }
    }

    /// Every system prompt the one captured run actually sent a model, macros
    /// already expanded. `contenox state show --raw` is where an operator reads
    /// what the model was told, which is the only way from outside to see what
    /// `{{tools}}` rendered to.
    pub fn captured_system_prompts(&self) -> Result<Vec<String>> {
        let requests = self.state_requests()?;
        let [only] = requests.as_slice() else {
            bail!("expected one captured run, got {requests:?}");
        };
        let out = self.run(["state", "show", only, "--raw"]);
        if !out.success() {
            bail!("contenox state show {only} --raw failed\n{}", out.render());
        }
        let captured: serde_json::Value = serde_json::from_str(&out.stdout)
            .with_context(|| format!("contenox state show --raw emitted\n{}", out.render()))?;
        let mut prompts = Vec::new();
        collect_system_prompts(&captured, &mut prompts);
        Ok(prompts)
    }

    pub fn doctor(&self) -> CmdOutput {
        self.run(["doctor"])
    }

    pub fn doctor_json(&self) -> Result<serde_json::Value> {
        let out = self.run(["doctor", "--json"]);
        serde_json::from_str(&out.stdout)
            .with_context(|| format!("contenox doctor --json emitted\n{}", out.render()))
    }
}

fn table_or_empty(text: &str, headers: &[&str]) -> Result<Table> {
    match Table::parse(text, headers) {
        Ok(table) => Ok(table),
        Err(err) => {
            if is_empty_state(text) {
                Ok(Table::empty(headers))
            } else {
                Err(err)
            }
        }
    }
}

fn is_empty_state(text: &str) -> bool {
    let first = text.lines().map(str::trim).find(|line| !line.is_empty());
    match first {
        None => true,
        Some(line) => line.starts_with("No ") || line.starts_with("(no "),
    }
}

fn parse_session_line(line: &str) -> Option<SessionRow> {
    let trimmed = line.trim();
    if trimmed.is_empty() {
        return None;
    }
    let current = trimmed.starts_with('*');
    let rest = trimmed.trim_start_matches('*').trim();
    let open = rest.rfind('(')?;
    let name = rest[..open].trim().to_string();
    let tail = &rest[open + 1..];
    let messages = tail
        .split_whitespace()
        .next()
        .and_then(|n| n.parse::<u32>().ok())?;
    Some(SessionRow {
        name,
        messages,
        current,
    })
}

fn labelled_fields(text: &str) -> BTreeMap<String, String> {
    let mut fields = BTreeMap::new();
    for line in text.lines() {
        if line.starts_with(char::is_whitespace) {
            continue;
        }
        let Some((label, value)) = line.split_once(':') else {
            continue;
        };
        if label.is_empty() || label.contains(' ') {
            continue;
        }
        fields.insert(label.trim().to_string(), value.trim().to_string());
    }
    fields
}

/// Walk a captured unit for the messages it holds; the shape nests differently
/// per handler, so the role is what a system prompt is found by.
fn collect_system_prompts(value: &serde_json::Value, into: &mut Vec<String>) {
    match value {
        serde_json::Value::Array(items) => {
            for item in items {
                collect_system_prompts(item, into);
            }
        }
        serde_json::Value::Object(fields) => {
            let is_system =
                fields.get("role").and_then(serde_json::Value::as_str) == Some("system");
            if let (true, Some(content)) = (
                is_system,
                fields.get("content").and_then(serde_json::Value::as_str),
            ) {
                let content = content.to_string();
                if !into.contains(&content) {
                    into.push(content);
                }
            }
            for nested in fields.values() {
                collect_system_prompts(nested, into);
            }
        }
        _ => {}
    }
}
