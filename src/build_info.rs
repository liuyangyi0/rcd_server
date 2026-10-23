//! Build identity only: reading this information never opens runtime files.
use serde::Serialize;

#[derive(Debug, Serialize)]
pub struct BuildInfo {
    pub schema_version: u32,
    pub component: &'static str,
    pub version: &'static str,
    pub commit: &'static str,
    pub built_at: &'static str,
    pub release_id: &'static str,
    pub source_sha256: &'static str,
    pub dirty: bool,
    pub target: String,
}

pub fn current() -> BuildInfo {
    BuildInfo {
        schema_version: 1,
        component: "lower",
        version: option_env!("RCD_BUILD_VERSION").unwrap_or(env!("CARGO_PKG_VERSION")),
        commit: option_env!("RCD_BUILD_COMMIT").unwrap_or("unknown"),
        built_at: option_env!("RCD_BUILD_TIME").unwrap_or("unknown"),
        release_id: option_env!("RCD_RELEASE_ID").unwrap_or("dev"),
        source_sha256: option_env!("RCD_SOURCE_SHA256").unwrap_or("unknown"),
        dirty: option_env!("RCD_BUILD_DIRTY") == Some("true"),
        target: format!("{}/{}", std::env::consts::OS, std::env::consts::ARCH),
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn identity_has_versioned_additive_fields() {
        let value = serde_json::to_value(super::current()).unwrap();
        assert_eq!(value["schema_version"], 1);
        assert_eq!(value["component"], "lower");
        for name in [
            "version",
            "commit",
            "release_id",
            "source_sha256",
            "built_at",
            "target",
        ] {
            assert!(!value[name].as_str().unwrap().is_empty());
        }
        assert!(value["dirty"].is_boolean());
    }
}
