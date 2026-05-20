//! 文件扫描模块。
//!
//! 扫描指定目录，返回文件名匹配正则表达式的文件路径列表。

use std::fs;
use std::io;
use std::path::{Path, PathBuf};

use regex::Regex;

/// 扫描 `directory` 下所有文件名匹配 `pattern` 的文件，返回路径列表。
///
/// # 参数
/// - `directory` - 要扫描的目录路径。
/// - `pattern`   - 正则表达式字符串（匹配文件名）。
pub fn read_and_process_files(directory: &Path, pattern: &str) -> io::Result<Vec<PathBuf>> {
    let re = Regex::new(pattern).map_err(|e| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("无效的正则表达式: {}", e),
        )
    })?;
    let mut matched_files = Vec::new();

    for entry in fs::read_dir(directory)? {
        let path = match entry {
            Ok(e) => e.path(),
            Err(_) => continue,
        };
        if is_matching_file(&path, &re) {
            matched_files.push(path);
        }
    }

    Ok(matched_files)
}

/// 判断路径是否为文件且文件名匹配正则。
fn is_matching_file(path: &Path, regex: &Regex) -> bool {
    path.is_file()
        && path
            .file_name()
            .and_then(|n| n.to_str())
            .is_some_and(|name| regex.is_match(name))
}
