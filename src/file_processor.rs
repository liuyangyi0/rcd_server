// file_processor.rs
use std::fs; // 引入文件系统操作模块
use std::io; // 引入IO操作模块
use std::path::{Path, PathBuf};
use regex::Regex; // 引入正则表达式库

// 公共函数，读取并处理符合条件的文件，并返回符合条件的文件列表
pub fn read_and_process_files(directory: &Path, pattern: &str) -> io::Result<Vec<PathBuf>> {
    let re = Regex::new(pattern).expect("正则表达式无效");
    let mut valid_files = Vec::new();  // 存储符合条件的文件路径
    //打印目录
    println!("directory: {:?}", directory);
    // 遍历目录中的文件
    let entries = fs::read_dir(directory)?;
    for entry in entries {
        let path = match entry {
            Ok(e) => e.path(),
            Err(_) => continue, // 忽略无法读取的文件项
        };

        if is_valid_file(&path, &re) {
            valid_files.push(path);
        }
    }
    Ok(valid_files)
}


// 辅助函数，判断文件是否符合正则表达式
fn is_valid_file(file_path: &Path, regex: &Regex) -> bool {
    file_path.is_file() && file_path.file_name()
        .and_then(|name| name.to_str())
        .map_or(false, |name| regex.is_match(name))
}


