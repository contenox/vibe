use anyhow::{Context, Result, bail};
use std::fs::File;
use std::os::unix::io::AsRawFd;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::OnceLock;
use std::time::SystemTime;

static BIN: OnceLock<std::result::Result<PathBuf, String>> = OnceLock::new();

pub fn exe_suffix() -> &'static str {
    if cfg!(windows) { ".exe" } else { "" }
}

pub fn repo_root() -> Result<PathBuf> {
    let mut dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    loop {
        if dir.join("go.mod").is_file() && dir.join("cmd").join("contenox").is_dir() {
            return Ok(dir);
        }
        if !dir.pop() {
            bail!(
                "no go.mod with cmd/contenox above {} — is this crate still inside the runtime checkout?",
                env!("CARGO_MANIFEST_DIR")
            );
        }
    }
}

pub fn contenox_bin() -> Result<PathBuf> {
    BIN.get_or_init(|| locate().map_err(|err| format!("{err:#}")))
        .clone()
        .map_err(anyhow::Error::msg)
}

fn locate() -> Result<PathBuf> {
    if let Some(raw) = std::env::var_os("CONTENOX_BIN") {
        let path = PathBuf::from(&raw);
        if !path.is_file() {
            bail!("CONTENOX_BIN={} is not a file", path.display());
        }
        return path
            .canonicalize()
            .with_context(|| format!("CONTENOX_BIN={}", path.display()));
    }

    let root = repo_root()?;
    let built = root.join("bin").join(format!("contenox{}", exe_suffix()));
    if std::env::var_os("CONTENOX_E2E_NO_BUILD").is_some() {
        if !built.is_file() {
            bail!(
                "CONTENOX_E2E_NO_BUILD is set but {} does not exist — run `task build` first, or set CONTENOX_BIN",
                built.display()
            );
        }
        return Ok(built);
    }
    build(&root, &built)?;
    Ok(built)
}

fn is_current(root: &Path, built: &Path) -> bool {
    if std::env::var_os("CONTENOX_E2E_REBUILD").is_some() {
        return false;
    }
    let Ok(binary) = built.metadata().and_then(|m| m.modified()) else {
        return false;
    };
    match newest_source(root) {
        Some(source) => binary >= source,
        None => false,
    }
}

fn newest_source(root: &Path) -> Option<SystemTime> {
    const SKIP: &[&str] = &[
        ".git",
        ".contenox",
        "bin",
        "dist",
        "node_modules",
        "target",
        "website",
    ];
    let mut newest: Option<SystemTime> = None;
    let mut stack = vec![root.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            let Ok(kind) = entry.file_type() else {
                continue;
            };
            if kind.is_dir() {
                if !SKIP.contains(&name.as_ref()) {
                    stack.push(entry.path());
                }
                continue;
            }
            let interesting = name.ends_with(".go") || name == "go.mod" || name == "go.sum";
            if !interesting {
                continue;
            }
            if let Ok(modified) = entry.metadata().and_then(|m| m.modified()) {
                newest = Some(match newest {
                    Some(current) if current >= modified => current,
                    _ => modified,
                });
            }
        }
    }
    newest
}

fn build(root: &Path, out: &Path) -> Result<PathBuf> {
    if is_current(root, out) {
        return Ok(out.to_path_buf());
    }
    let dir = out.parent().unwrap_or(root);
    std::fs::create_dir_all(dir).with_context(|| format!("create {}", dir.display()))?;

    let lock_path = dir.join(".contenox-e2e-build.lock");
    let lock =
        File::create(&lock_path).with_context(|| format!("create {}", lock_path.display()))?;
    let locked = unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX) } == 0;
    if !locked {
        bail!("could not lock {}", lock_path.display());
    }

    if is_current(root, out) {
        return Ok(out.to_path_buf());
    }

    let staged = dir.join(format!(".contenox-e2e-build.{}", std::process::id()));
    let status = Command::new("go")
        .arg("build")
        .arg("-o")
        .arg(&staged)
        .arg(root.join("cmd").join("contenox"))
        .current_dir(root)
        .env("CGO_ENABLED", "0")
        .status()
        .context("running `go build` — install Go, or set CONTENOX_BIN to a prebuilt contenox")?;
    if !status.success() {
        let _ = std::fs::remove_file(&staged);
        bail!("`go build ./cmd/contenox` failed with {status}");
    }
    std::fs::rename(&staged, out)
        .with_context(|| format!("install {} -> {}", staged.display(), out.display()))?;
    Ok(out.to_path_buf())
}
