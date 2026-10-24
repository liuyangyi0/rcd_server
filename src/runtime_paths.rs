//! Explicit runtime locations, with the old executable-relative defaults.
//! This module never migrates or creates user configuration.
use std::collections::HashSet;
use std::ffi::OsString;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

static PATHS: OnceLock<RuntimePaths> = OnceLock::new();

#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Overrides {
    pub config: Option<PathBuf>,
    pub state: Option<PathBuf>,
    pub pki: Option<PathBuf>,
    pub log: Option<PathBuf>,
}

#[derive(Debug, Clone)]
pub struct RuntimePaths {
    pub config: PathBuf,
    pub state: PathBuf,
    pub pki: PathBuf,
    pub log_override: Option<PathBuf>,
    pub exe_dir: PathBuf,
    explicit_config: bool,
    explicit_state: bool,
    explicit_pki: bool,
}

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.into())
}

impl Overrides {
    pub fn parse(args: impl IntoIterator<Item = OsString>) -> io::Result<Self> {
        let mut result = Self::default();
        let mut args = args.into_iter();
        let mut seen = HashSet::new();
        while let Some(arg) = args.next() {
            let text = arg.to_str().ok_or_else(|| invalid("invalid option name"))?;
            let (name, inline) = text
                .split_once('=')
                .map_or((text, None), |(k, v)| (k, Some(v)));
            let slot = match name {
                "--config-dir" => &mut result.config,
                "--state-dir" => &mut result.state,
                "--pki-dir" => &mut result.pki,
                "--log-dir" => &mut result.log,
                _ => return Err(invalid(format!("unknown argument: {name}"))),
            };
            if !seen.insert(name.to_string()) {
                return Err(invalid(format!("duplicate argument: {name}")));
            }
            let value = match inline {
                Some(value) => OsString::from(value),
                None => args
                    .next()
                    .ok_or_else(|| invalid(format!("missing value for {name}")))?,
            };
            if value.is_empty() || value.to_string_lossy().starts_with("--") {
                return Err(invalid(format!("missing path for {name}")));
            }
            *slot = Some(PathBuf::from(value));
        }
        Ok(result)
    }

    fn with_environment(self) -> io::Result<Self> {
        self.with_lookup(|name| std::env::var_os(name))
    }

    fn with_lookup(mut self, lookup: impl Fn(&str) -> Option<OsString>) -> io::Result<Self> {
        for (slot, name) in [
            (&mut self.config, "RCD_CONFIG_DIR"),
            (&mut self.state, "RCD_STATE_DIR"),
            (&mut self.pki, "RCD_PKI_DIR"),
            (&mut self.log, "RCD_LOG_DIR"),
        ] {
            if slot.is_none() {
                if let Some(value) = lookup(name) {
                    if value.is_empty() {
                        return Err(invalid(format!("{name} cannot be empty")));
                    }
                    *slot = Some(value.into());
                }
            }
        }
        Ok(self)
    }
}

impl RuntimePaths {
    pub fn resolve(exe: &Path, cwd: &Path, options: Overrides) -> io::Result<Self> {
        let exe_dir = exe
            .parent()
            .ok_or_else(|| invalid("executable has no parent"))?
            .to_path_buf();
        let absolute = |value: PathBuf| {
            if value.is_absolute() {
                value
            } else {
                cwd.join(value)
            }
        };
        let explicit_config = options.config.is_some();
        let explicit_state = options.state.is_some();
        let explicit_pki = options.pki.is_some();
        let config = options
            .config
            .map(absolute)
            .unwrap_or_else(|| exe_dir.join("config"));
        let state = options
            .state
            .map(absolute)
            .unwrap_or_else(|| config.clone());
        Ok(Self {
            config,
            state,
            exe_dir,
            explicit_config,
            explicit_state,
            explicit_pki,
            pki: options.pki.map(absolute).unwrap_or_else(|| cwd.join("pki")),
            log_override: options.log.map(absolute),
        })
    }

    pub fn validate(&self) -> io::Result<()> {
        for (explicit, path) in [
            (self.explicit_config, &self.config),
            (self.explicit_state, &self.state),
            (self.explicit_pki, &self.pki),
        ] {
            if explicit && !path.is_dir() {
                return Err(invalid(format!(
                    "explicit runtime directory must already exist: {}",
                    path.display()
                )));
            }
        }
        // A new empty state directory must not silently discard old identities,
        // acquisition switches or command evidence. Migration is explicit.
        if self.explicit_state
            && std::fs::canonicalize(&self.config)? != std::fs::canonicalize(&self.state)?
        {
            for file in [
                "operations.json",
                "station_versions.json",
                ".station_versions.json.bak",
                "command-journal.sqlite",
                "maintenance.sqlite",
            ] {
                if self.config.join(file).exists() && !self.state.join(file).is_file() {
                    return Err(invalid(format!(
                        "state migration required before startup: {file}"
                    )));
                }
            }
        }
        Ok(())
    }

    pub fn lock_paths(&self) -> Vec<PathBuf> {
        let legacy = self.exe_dir.join(".rcd_server.lock");
        let mut paths = vec![
            self.config.join(".rcd_server.lock"),
            self.state.join(".rcd_server.lock"),
        ];
        // Continue respecting an existing old-version lock. New explicit
        // installations do not need write access to /opt/rcd/bin.
        if (!self.explicit_config && !self.explicit_state) || legacy.exists() {
            paths.push(legacy);
        }
        paths
    }
}

pub fn initialize(options: Overrides) -> io::Result<()> {
    let paths = RuntimePaths::resolve(
        &std::env::current_exe()?,
        &std::env::current_dir()?,
        options.with_environment()?,
    )?;
    paths.validate()?;
    PATHS
        .set(paths)
        .map_err(|_| invalid("runtime paths already initialized"))
}

pub fn current() -> io::Result<RuntimePaths> {
    if let Some(paths) = PATHS.get() {
        return Ok(paths.clone());
    }
    RuntimePaths::resolve(
        &std::env::current_exe()?,
        &std::env::current_dir()?,
        Overrides::default().with_environment()?,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn cli_overrides_environment_and_empty_environment_is_not_a_default() {
        let cli = Overrides::parse([OsString::from("--config-dir=cli-config")]).unwrap();
        let resolved = cli
            .with_lookup(|name| {
                Some(OsString::from(if name == "RCD_CONFIG_DIR" {
                    ""
                } else {
                    "environment-path"
                }))
            })
            .unwrap();
        assert_eq!(resolved.config, Some(PathBuf::from("cli-config")));
        assert_eq!(resolved.state, Some(PathBuf::from("environment-path")));
        assert!(Overrides::default()
            .with_lookup(|_| Some(OsString::new()))
            .is_err());
    }

    #[test]
    fn explicit_installation_uses_stable_locks_without_writing_binary_directory() {
        let root = tempfile::tempdir().unwrap();
        let bin = root.path().join("bin");
        std::fs::create_dir(&bin).unwrap();
        let paths = RuntimePaths::resolve(
            &bin.join("rcd_server"),
            root.path(),
            Overrides {
                config: Some(root.path().join("config")),
                state: Some(root.path().join("state")),
                ..Default::default()
            },
        )
        .unwrap();
        assert!(!paths.lock_paths().contains(&bin.join(".rcd_server.lock")));
        std::fs::write(bin.join(".rcd_server.lock"), "old-version-lock").unwrap();
        assert!(paths.lock_paths().contains(&bin.join(".rcd_server.lock")));
    }
    #[test]
    fn legacy_locations_are_unchanged() {
        let root = tempfile::tempdir().unwrap();
        let exe = root.path().join("bin/rcd_server");
        let cwd = root.path().join("working");
        let paths = RuntimePaths::resolve(&exe, &cwd, Overrides::default()).unwrap();
        assert_eq!(paths.config, root.path().join("bin/config"));
        assert_eq!(paths.state, paths.config);
        assert_eq!(paths.pki, cwd.join("pki"));
        assert!(paths.log_override.is_none());
    }
    #[test]
    fn relative_explicit_paths_use_working_directory() {
        let root = tempfile::tempdir().unwrap();
        let options = Overrides::parse(
            [
                "--config-dir",
                "my config",
                "--state-dir=state",
                "--pki-dir=pki",
                "--log-dir=logs",
            ]
            .map(OsString::from),
        )
        .unwrap();
        let paths =
            RuntimePaths::resolve(&root.path().join("bin/rcd_server"), root.path(), options)
                .unwrap();
        assert_eq!(paths.config, root.path().join("my config"));
        assert_eq!(paths.state, root.path().join("state"));
        assert_eq!(paths.log_override, Some(root.path().join("logs")));
        assert!(paths.validate().is_err());
        assert!(!paths.config.exists());
    }
    #[test]
    fn invalid_or_ambiguous_arguments_are_rejected() {
        for args in [
            vec!["--config-dir"],
            vec!["--log-dir="],
            vec!["--bogus=x"],
            vec!["--state-dir=a", "--state-dir=b"],
            vec!["--config-dir", "--state-dir=x"],
        ] {
            assert!(Overrides::parse(args.into_iter().map(OsString::from)).is_err());
        }
    }
    #[test]
    fn explicit_state_cannot_silently_reset_existing_data() {
        let root = tempfile::tempdir().unwrap();
        let config = root.path().join("config");
        let state = root.path().join("state");
        std::fs::create_dir(&config).unwrap();
        std::fs::create_dir(&state).unwrap();
        std::fs::write(config.join("operations.json"), "original-user-data").unwrap();
        let paths = RuntimePaths::resolve(
            &root.path().join("rcd_server"),
            root.path(),
            Overrides {
                config: Some(config.clone()),
                state: Some(state.clone()),
                ..Default::default()
            },
        )
        .unwrap();
        assert!(paths
            .validate()
            .unwrap_err()
            .to_string()
            .contains("migration required"));
        assert!(!state.join("operations.json").exists());
        std::fs::copy(
            config.join("operations.json"),
            state.join("operations.json"),
        )
        .unwrap();
        paths.validate().unwrap();
    }
}
