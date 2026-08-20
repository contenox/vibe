use crate::acp::Acp;
use crate::binary::contenox_bin;
use crate::cmd::{Cmd, CmdOutput, Running};
use crate::pty::Pty;
use crate::script::Script;
use anyhow::{Context, Result, bail};
use std::ffi::{OsStr, OsString};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

static SEQUENCE: AtomicU64 = AtomicU64::new(0);

const SCRUBBED_PREFIXES: &[&str] = &["CONTENOX_", "XDG_"];
const SCRUBBED_NAMES: &[&str] = &[
    "OLLAMA_API_KEY",
    "OPENAI_API_KEY",
    "ANTHROPIC_API_KEY",
    "GEMINI_API_KEY",
];

pub struct Instance {
    root: PathBuf,
    home: PathBuf,
    work: PathBuf,
    bin: PathBuf,
    env: Vec<(OsString, Option<OsString>)>,
    keep: bool,
}

impl Instance {
    pub fn new() -> Result<Self> {
        Self::named("case")
    }

    pub fn named(label: &str) -> Result<Self> {
        let bin = contenox_bin()?;
        let stamp = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let unique = SEQUENCE.fetch_add(1, Ordering::SeqCst);
        let safe: String = label
            .chars()
            .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
            .collect();
        let root = std::env::temp_dir().join(format!(
            "contenox-e2e-{}-{safe}-{unique}-{stamp}",
            std::process::id()
        ));
        let home = root.join("home");
        let work = root.join("work");
        let tmp = root.join("tmp");
        for dir in [&home, &work, &tmp] {
            std::fs::create_dir_all(dir).with_context(|| format!("create {}", dir.display()))?;
        }

        guard_real_home(&home)?;

        let mut env: Vec<(OsString, Option<OsString>)> = Vec::new();
        for (key, _) in std::env::vars_os() {
            let name = key.to_string_lossy().into_owned();
            let scrub = SCRUBBED_PREFIXES.iter().any(|p| name.starts_with(p))
                || SCRUBBED_NAMES.contains(&name.as_str());
            if scrub {
                env.push((key, None));
            }
        }
        env.push(("HOME".into(), Some(home.clone().into_os_string())));
        env.push(("USERPROFILE".into(), Some(home.clone().into_os_string())));
        env.push(("TMPDIR".into(), Some(tmp.into_os_string())));
        // Pinned for the same reason as HOME: an interactive shell exports
        // these and a CI runner exports neither, so anything that reads the
        // terminal description — the sandbox's env allowlist, the colour
        // ladder — would assert against the developer's shell and diverge in
        // CI. A pinned value is not a TTY; colour still needs one.
        env.push(("TERM".into(), Some("xterm-256color".into())));
        env.push(("COLORTERM".into(), Some("truecolor".into())));

        Ok(Instance {
            root,
            home,
            work,
            bin,
            env,
            keep: std::env::var_os("CONTENOX_E2E_KEEP").is_some(),
        })
    }

    pub fn started() -> Result<Self> {
        let instance = Self::new()?;
        instance.run(["init"]).ok();
        Ok(instance)
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn home(&self) -> &Path {
        &self.home
    }

    pub fn work(&self) -> &Path {
        &self.work
    }

    pub fn bin(&self) -> &Path {
        &self.bin
    }

    pub fn keep_on_drop(&mut self) {
        self.keep = true;
    }

    pub fn set_env(&mut self, key: impl AsRef<OsStr>, value: impl AsRef<OsStr>) {
        self.env.push((
            key.as_ref().to_os_string(),
            Some(value.as_ref().to_os_string()),
        ));
    }

    pub fn cmd<I, S>(&self, args: I) -> Cmd
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        let mut cmd = Cmd::new(&self.bin, &self.work).args(args);
        for (key, value) in &self.env {
            cmd = match value {
                Some(value) => cmd.env(key, value),
                None => cmd.env_remove(key),
            };
        }
        cmd
    }

    pub fn run<I, S>(&self, args: I) -> CmdOutput
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        self.cmd(args)
            .output()
            .unwrap_or_else(|err| panic!("{err:#}"))
    }

    pub fn start<I, S>(&self, args: I) -> Result<Running>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        self.cmd(args).start()
    }

    pub fn pty<I, S>(&self, args: I) -> Result<Pty>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        Pty::spawn(self.cmd(args))
    }

    /// Spawn an ACP surface and speak the protocol to it over stdio, the way an
    /// editor does. Pass the whole argv: `["acp"]`, `["acp", "--auto"]`, `["acpx"]`.
    pub fn acp<I, S>(&self, args: I) -> Result<Acp>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        Acp::spawn(self.cmd(args))
    }

    pub fn init(&self) -> CmdOutput {
        self.run(["init"])
    }

    pub fn write_file(&self, relative: impl AsRef<Path>, contents: &str) -> Result<PathBuf> {
        let path = self.work.join(relative);
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("create {}", parent.display()))?;
        }
        std::fs::write(&path, contents).with_context(|| format!("write {}", path.display()))?;
        Ok(path)
    }

    pub fn read_file(&self, relative: impl AsRef<Path>) -> Result<String> {
        let path = self.work.join(relative);
        std::fs::read_to_string(&path).with_context(|| format!("read {}", path.display()))
    }

    pub fn generated(&self, name: &str) -> PathBuf {
        self.work.join(".contenox").join(".generated").join(name)
    }

    pub fn home_file(&self, relative: impl AsRef<Path>) -> PathBuf {
        self.home.join(".contenox").join(relative)
    }

    /// Write under the scratch `~/.contenox/`, creating the parents — the other
    /// place declarations live, for an agent an operator wants everywhere.
    pub fn write_home_file(&self, relative: impl AsRef<Path>, contents: &str) -> Result<PathBuf> {
        let path = self.home_file(relative);
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("create {}", parent.display()))?;
        }
        std::fs::write(&path, contents).with_context(|| format!("write {}", path.display()))?;
        Ok(path)
    }

    pub fn write_script(&self, name: &str, script: &Script) -> Result<PathBuf> {
        let path = self.work.join(name);
        std::fs::write(&path, script.to_json())
            .with_context(|| format!("write {}", path.display()))?;
        Ok(path)
    }

    pub fn scripted_backend(&self, name: &str, script: &Script) -> Result<PathBuf> {
        let path = self.write_script(&format!("{name}.json"), script)?;
        self.run([
            OsStr::new("backend"),
            OsStr::new("add"),
            OsStr::new(name),
            OsStr::new("--type"),
            OsStr::new("scripted-test"),
            OsStr::new("--script"),
            path.as_os_str(),
        ])
        .ok();
        self.run(["config", "set", "default-provider", "scripted-test"])
            .ok();
        self.run(["config", "set", "default-model", "scripted-test"])
            .ok();
        Ok(path)
    }

    pub fn scripted(&self, script: &Script) -> Result<PathBuf> {
        self.scripted_backend("scripted", script)
    }

    pub fn wait_for(
        &self,
        timeout: Duration,
        mut ready: impl FnMut(&Instance) -> bool,
    ) -> Result<()> {
        let deadline = std::time::Instant::now() + timeout;
        loop {
            if ready(self) {
                return Ok(());
            }
            if std::time::Instant::now() >= deadline {
                bail!("condition still false after {timeout:?}");
            }
            std::thread::sleep(Duration::from_millis(200));
        }
    }
}

impl Drop for Instance {
    fn drop(&mut self) {
        if self.keep {
            eprintln!("contenox-e2e: kept {}", self.root.display());
            return;
        }
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

fn guard_real_home(scratch: &Path) -> Result<()> {
    let Some(real) = std::env::var_os("HOME") else {
        return Ok(());
    };
    let real = PathBuf::from(real);
    if real == scratch || scratch.starts_with(real.join(".contenox")) {
        bail!(
            "refusing to run: the scratch home {} is inside the real ~/.contenox",
            scratch.display()
        );
    }
    Ok(())
}
