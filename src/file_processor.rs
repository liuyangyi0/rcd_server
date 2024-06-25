// file_processor.rs
use std::fs; // 引入文件系统操作模块
use std::io; // 引入IO操作模块
use std::path::Path; // 引入路径操作模块
use regex::Regex; // 引入正则表达式库

// 公共函数，读取并处理符合条件的文件
pub fn read_and_process_files(directory: &Path, pattern: &str) -> io::Result<()> {
    let re = Regex::new(pattern).expect("正则表达式无效");

    // 遍历目录中的文件
    for entry in fs::read_dir(directory)? {
        let path = entry?.path();
        if is_valid_file(&path, &re) { // 验证文件是否有效
            println!("处理文件：{}", path.display());
            let contents = process_file(&path)?; // 处理文件并获取内容
            println!("文件内容：{}", contents);
        }
    }
    Ok(())
}

// 私有函数，处理单个文件，读取其内容
fn process_file(file_path: &Path) -> io::Result<String> {
    let mut file = fs::File::open(file_path)?;
    let mut contents = String::new();
    file.read_to_string(&mut contents)?; // 读取文件到字符串
    Ok(contents)
}

// 辅助函数，判断文件是否符合正则表达式
fn is_valid_file(file_path: &Path, regex: &Regex) -> bool {
    file_path.is_file() && file_path.file_name()
        .and_then(|name| name.to_str())
        .map_or(false, |name| regex.is_match(name))
}


//测试
#[cfg(test)]
mod tests {
    use super::*;
    use std::fs::File;
    use std::io::Write;
    use std::env;

    #[test]
    fn test_read_and_process_files() -> Result<(), Box<dyn Error>>{
        let exe_path = env::current_exe()?; // 获取当前可执行文件路径
        let exe_dir = exe_path.parent().ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "无法获取可执行文件目录"))?;
    
        let config_dir = exe_dir.join("config"); // 构建配置文件目录路径
    
        // 调用函数处理所有符合正则表达式的文件
        if let Err(e) = read_and_process_files(&config_dir, r"^rcd配置\-\d+\.csv$") {
            eprintln!("处理文件时发生错误：{}", e); // 错误处理
        }
    }
}
