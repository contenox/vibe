use anyhow::{Context, Result, bail};
use std::ffi::{OsStr, OsString};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

#[derive(Debug, Clone)]
pub struct CmdOutput {
    pub argv: Vec<String>,
    pub code: Option<i32>,
    pub stdout: String,
    pub stderr: String,
}

impl CmdOutput {
    pub fn success(&self) -> bool {
        self.code == Some(0)
    }

    pub fn ok(self) -> Self {
        self.expect_code(0)
    }

    pub fn expect_code(self, want: i32) -> Self {
        if self.code != Some(want) {
            panic!(
                "{} exited {:?}, wanted {want}\n{}",
                self.argv.join(" "),
                self.code,
                self.render()
            );
        }
        self
    }

    pub fn expect_failure(self) -> Self {
        if self.success() {
            panic!(
                "{} unexpectedly succeeded\n{}",
                self.argv.join(" "),
                self.render()
            );
        }
        self
    }

    pub fn stdout_has(&self, needle: &str) -> bool {
        self.stdout.contains(needle)
    }

    pub fn stderr_has(&self, needle: &str) -> bool {
        self.stderr.contains(needle)
    }

    pub fn expect_stdout(self, needle: &str) -> Self {
        if !self.stdout_has(needle) {
            panic!(
                "{} stdout does not contain {needle:?}\n{}",
                self.argv.join(" "),
                self.render()
            );
        }
        self
    }

    pub fn expect_stderr(self, needle: &str) -> Self {
        if !self.stderr_has(needle) {
            panic!(
                "{} stderr does not contain {needle:?}\n{}",
                self.argv.join(" "),
                self.render()
            );
        }
        self
    }

    pub fn refute_stdout(self, needle: &str) -> Self {
        if self.stdout_has(needle) {
            panic!(
                "{} stdout unexpectedly contains {needle:?}\n{}",
                self.argv.join(" "),
                self.render()
            );
        }
        self
    }

    pub fn stdout_lines(&self) -> Vec<&str> {
        self.stdout.lines().collect()
    }

    pub fn stdout_trimmed(&self) -> &str {
        self.stdout.trim()
    }

    pub fn notices(&self) -> String {
        self.stderr
            .lines()
            .filter(|line| !is_slog_line(line))
            .collect::<Vec<_>>()
            .join("\n")
    }

    pub fn render(&self) -> String {
        format!(
            "--- stdout ---\n{}\n--- stderr (log lines dropped) ---\n{}\n",
            self.stdout,
            self.notices()
        )
    }
}

fn is_slog_line(line: &str) -> bool {
    line.starts_with("time=") && line.contains(" level=")
}

pub struct Cmd {
    program: PathBuf,
    args: Vec<OsString>,
    cwd: PathBuf,
    env: Vec<(OsString, Option<OsString>)>,
    stdin: Option<Vec<u8>>,
    timeout: Duration,
}

impl Cmd {
    pub fn new(program: impl AsRef<Path>, cwd: impl AsRef<Path>) -> Self {
        Self {
            program: program.as_ref().to_path_buf(),
            args: Vec::new(),
            cwd: cwd.as_ref().to_path_buf(),
            env: Vec::new(),
            stdin: None,
            timeout: Duration::from_secs(120),
        }
    }

    pub fn arg(mut self, arg: impl AsRef<OsStr>) -> Self {
        self.args.push(arg.as_ref().to_os_string());
        self
    }

    pub fn args<I, S>(mut self, args: I) -> Self
    where
        I: IntoIterator<Item = S>,
        S: AsRef<OsStr>,
    {
        self.args
            .extend(args.into_iter().map(|a| a.as_ref().to_os_string()));
        self
    }

    pub fn env(mut self, key: impl AsRef<OsStr>, value: impl AsRef<OsStr>) -> Self {
        self.env.push((
            key.as_ref().to_os_string(),
            Some(value.as_ref().to_os_string()),
        ));
        self
    }

    pub fn env_remove(mut self, key: impl AsRef<OsStr>) -> Self {
        self.env.push((key.as_ref().to_os_string(), None));
        self
    }

    pub fn cwd(mut self, dir: impl AsRef<Path>) -> Self {
        self.cwd = dir.as_ref().to_path_buf();
        self
    }

    pub fn stdin(mut self, bytes: impl Into<Vec<u8>>) -> Self {
        self.stdin = Some(bytes.into());
        self
    }

    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    pub fn argv(&self) -> Vec<String> {
        let mut out = vec![self.program.display().to_string()];
        out.extend(self.args.iter().map(|a| a.to_string_lossy().into_owned()));
        out
    }

    pub fn envs(&self) -> &[(OsString, Option<OsString>)] {
        &self.env
    }

    pub fn program(&self) -> &Path {
        &self.program
    }

    pub fn arg_list(&self) -> &[OsString] {
        &self.args
    }

    pub fn dir(&self) -> &Path {
        &self.cwd
    }

    fn command(&self) -> Command {
        let mut command = Command::new(&self.program);
        command.args(&self.args).current_dir(&self.cwd);
        for (key, value) in &self.env {
            match value {
                Some(value) => command.env(key, value),
                None => command.env_remove(key),
            };
        }
        command
    }

    pub fn start(self) -> Result<Running> {
        let argv = self.argv();
        let mut command = self.command();
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let mut child = command
            .spawn()
            .with_context(|| format!("spawn {}", argv.join(" ")))?;

        let mut stdin = child.stdin.take();
        if let (Some(bytes), Some(mut handle)) = (self.stdin.clone(), stdin.take()) {
            std::thread::spawn(move || {
                let _ = handle.write_all(&bytes);
            });
        }

        let stdout = drain(child.stdout.take());
        let stderr = drain(child.stderr.take());
        Ok(Running {
            argv,
            child,
            stdin,
            stdout: Some(stdout),
            stderr: Some(stderr),
            timeout: self.timeout,
        })
    }

    pub fn output(self) -> Result<CmdOutput> {
        let timeout = self.timeout;
        self.start()?.wait_timeout(timeout)
    }
}

fn drain<R: Read + Send + 'static>(source: Option<R>) -> JoinHandle<Vec<u8>> {
    std::thread::spawn(move || {
        let mut buf = Vec::new();
        if let Some(mut source) = source {
            let _ = source.read_to_end(&mut buf);
        }
        buf
    })
}

pub struct Running {
    argv: Vec<String>,
    child: Child,
    stdin: Option<std::process::ChildStdin>,
    stdout: Option<JoinHandle<Vec<u8>>>,
    stderr: Option<JoinHandle<Vec<u8>>>,
    timeout: Duration,
}

impl Running {
    pub fn argv(&self) -> &[String] {
        &self.argv
    }

    pub fn id(&self) -> u32 {
        self.child.id()
    }

    pub fn write_stdin(&mut self, bytes: &[u8]) -> Result<()> {
        let handle = self
            .stdin
            .as_mut()
            .context("stdin was already closed on this process")?;
        handle.write_all(bytes)?;
        handle.flush()?;
        Ok(())
    }

    pub fn close_stdin(&mut self) {
        self.stdin.take();
    }

    pub fn signal(&self, signal: i32) {
        unsafe { libc::kill(self.child.id() as libc::pid_t, signal) };
    }

    pub fn interrupt(&self) {
        self.signal(libc::SIGINT);
    }

    pub fn kill(&mut self) {
        let _ = self.child.kill();
    }

    pub fn wait(self) -> Result<CmdOutput> {
        let timeout = self.timeout;
        self.wait_timeout(timeout)
    }

    pub fn wait_timeout(mut self, timeout: Duration) -> Result<CmdOutput> {
        let deadline = Instant::now() + timeout;
        let code = loop {
            match self.child.try_wait()? {
                Some(status) => break status.code(),
                None if Instant::now() >= deadline => {
                    let _ = self.child.kill();
                    let _ = self.child.wait();
                    let partial = self.harvest(None);
                    bail!(
                        "{} did not exit within {:?}; killed it\n{}",
                        self.argv.join(" "),
                        timeout,
                        partial.render()
                    );
                }
                None => std::thread::sleep(Duration::from_millis(20)),
            }
        };
        Ok(self.harvest(code))
    }

    fn harvest(&mut self, code: Option<i32>) -> CmdOutput {
        self.stdin.take();
        let stdout = self
            .stdout
            .take()
            .map(|h| h.join().unwrap_or_default())
            .unwrap_or_default();
        let stderr = self
            .stderr
            .take()
            .map(|h| h.join().unwrap_or_default())
            .unwrap_or_default();
        CmdOutput {
            argv: self.argv.clone(),
            code,
            stdout: String::from_utf8_lossy(&stdout).into_owned(),
            stderr: String::from_utf8_lossy(&stderr).into_owned(),
        }
    }
}

impl Drop for Running {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
