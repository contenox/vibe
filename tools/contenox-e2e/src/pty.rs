use crate::cmd::Cmd;
use anyhow::{Context, Result, bail};
use std::io::{Read, Write};
use std::os::fd::{FromRawFd, OwnedFd};
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

pub struct Pty {
    argv: Vec<String>,
    master: std::fs::File,
    child: Child,
    seen: Arc<Mutex<Vec<u8>>>,
}

impl Pty {
    pub fn spawn(cmd: Cmd) -> Result<Pty> {
        Pty::spawn_sized(cmd, 40, 120)
    }

    pub fn spawn_sized(cmd: Cmd, rows: u16, cols: u16) -> Result<Pty> {
        let argv = cmd.argv();
        let (master, slave) = open_pty(rows, cols)?;

        let mut command = Command::new(cmd.program());
        command.args(cmd.arg_list()).current_dir(cmd.dir());
        for (key, value) in cmd.envs() {
            match value {
                Some(value) => command.env(key, value),
                None => command.env_remove(key),
            };
        }
        // Pinned, not inherited: the colour ladder beam picks is read off these
        // two, so a developer's shell (COLORTERM=truecolor) and a CI runner
        // (neither set) would otherwise drive the same assertion to different
        // profiles — truecolor here, ANSI256 there.
        command.env("TERM", "xterm-256color");
        command.env("COLORTERM", "truecolor");

        let stdin = slave.try_clone().context("clone the pty slave for stdin")?;
        let stdout = slave
            .try_clone()
            .context("clone the pty slave for stdout")?;
        let stderr = slave
            .try_clone()
            .context("clone the pty slave for stderr")?;
        command
            .stdin(Stdio::from(stdin))
            .stdout(Stdio::from(stdout))
            .stderr(Stdio::from(stderr));

        unsafe {
            command.pre_exec(|| {
                if libc::setsid() < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                if libc::ioctl(0, libc::TIOCSCTTY, 0) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }

        let child = command
            .spawn()
            .with_context(|| format!("spawn {} under a pty", argv.join(" ")))?;
        drop(slave);

        let master = std::fs::File::from(master);
        let reader = master
            .try_clone()
            .context("clone the pty master for reading")?;
        let seen = Arc::new(Mutex::new(Vec::new()));
        let sink = Arc::clone(&seen);
        std::thread::spawn(move || {
            let mut reader = reader;
            let mut buf = [0u8; 8192];
            loop {
                match reader.read(&mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => sink
                        .lock()
                        .expect("pty buffer")
                        .extend_from_slice(&buf[..n]),
                }
            }
        });

        Ok(Pty {
            argv,
            master,
            child,
            seen,
        })
    }

    pub fn send(&mut self, bytes: impl AsRef<[u8]>) -> Result<()> {
        self.master.write_all(bytes.as_ref())?;
        self.master.flush()?;
        Ok(())
    }

    pub fn send_line(&mut self, line: &str) -> Result<()> {
        self.send(line.as_bytes())?;
        self.send(b"\r")
    }

    pub fn send_ctrl(&mut self, letter: char) -> Result<()> {
        let upper = letter.to_ascii_uppercase() as u8;
        if !(b'A'..=b'_').contains(&upper) {
            bail!("{letter:?} has no control code");
        }
        self.send([upper - b'@'])
    }

    pub fn raw(&self) -> Vec<u8> {
        self.seen.lock().expect("pty buffer").clone()
    }

    pub fn screen(&self) -> String {
        strip_ansi(&String::from_utf8_lossy(&self.raw()))
    }

    pub fn wait_for(&self, needle: &str, timeout: Duration) -> Result<String> {
        let deadline = Instant::now() + timeout;
        loop {
            let screen = self.screen();
            if screen.contains(needle) {
                return Ok(screen);
            }
            if Instant::now() >= deadline {
                bail!(
                    "{} never rendered {needle:?} within {timeout:?}\n--- screen ---\n{screen}\n",
                    self.argv.join(" ")
                );
            }
            std::thread::sleep(Duration::from_millis(50));
        }
    }

    pub fn interrupt(&self) {
        self.signal(libc::SIGINT);
    }

    // What the kernel sends the foreground group when its terminal window closes.
    pub fn hangup(&self) {
        self.signal(libc::SIGHUP);
    }

    fn signal(&self, signal: libc::c_int) {
        unsafe { libc::kill(-(self.child.id() as libc::pid_t), signal) };
    }

    pub fn wait_exit(&mut self, timeout: Duration) -> Result<Option<i32>> {
        let deadline = Instant::now() + timeout;
        loop {
            if let Some(status) = self.child.try_wait()? {
                return Ok(status.code());
            }
            if Instant::now() >= deadline {
                bail!(
                    "{} was still running after {timeout:?}\n--- screen ---\n{}\n",
                    self.argv.join(" "),
                    self.screen()
                );
            }
            std::thread::sleep(Duration::from_millis(50));
        }
    }
}

impl Drop for Pty {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn open_pty(rows: u16, cols: u16) -> Result<(OwnedFd, OwnedFd)> {
    let mut master: libc::c_int = -1;
    let mut slave: libc::c_int = -1;
    let size = libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    let rc = unsafe {
        libc::openpty(
            &mut master,
            &mut slave,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            &size as *const libc::winsize as *mut libc::winsize,
        )
    };
    if rc != 0 {
        return Err(std::io::Error::last_os_error()).context("openpty");
    }
    unsafe { Ok((OwnedFd::from_raw_fd(master), OwnedFd::from_raw_fd(slave))) }
}

pub fn strip_ansi(text: &str) -> String {
    let chars: Vec<char> = text.chars().collect();
    let mut out = String::with_capacity(text.len());
    let mut i = 0usize;
    while i < chars.len() {
        if chars[i] != '\u{1b}' {
            out.push(chars[i]);
            i += 1;
            continue;
        }
        i += 1;
        match chars.get(i) {
            Some('[') => {
                i += 1;
                while i < chars.len() && !chars[i].is_ascii_alphabetic() && chars[i] != '@' {
                    i += 1;
                }
                i += 1;
            }
            Some(']') => {
                i += 1;
                while i < chars.len() {
                    if chars[i] == '\u{7}' {
                        i += 1;
                        break;
                    }
                    if chars[i] == '\u{1b}' && chars.get(i + 1) == Some(&'\\') {
                        i += 2;
                        break;
                    }
                    i += 1;
                }
            }
            Some(_) => i += 1,
            None => {}
        }
    }
    out
}
