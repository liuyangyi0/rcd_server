use serde::Deserialize;


// 定义枚举类型 RunLocation，表示程序是主还是备用
#[derive(Deserialize, Debug, Clone,PartialEq)]
pub enum RunLocation {
    #[serde(rename = "primary")]
    Primary,
    #[serde(rename = "secondary")]
    Secondary,
}

// 定义结构体 Server，包含主备用服务器的IP地址和程序运行位置
#[derive(Deserialize, Debug, Clone)]
pub struct Server {
    pub primary_ip: String,
    pub secondary_ip: String,
    pub run_on: RunLocation, //程序是主还是备用
    //程序当前是主还是备用
    pub current_run: RunLocation,
}

#[derive(Deserialize, Debug, Clone)]
pub struct Serial {
    pub baud_rate: u32,
    pub data_bits: u32,
    pub parity: String,
    pub stop_bits: u32,
    pub flow_control: String,
}

#[derive(Deserialize, Debug, Clone)]
pub struct Settings {
    pub timeout: u32,
}

#[derive(Deserialize, Debug, Clone)]
pub struct Config {
    pub server: Server,
    pub serial: Serial,
    pub settings: Settings,
}

pub fn init_config() -> Result<Config, Box<dyn std::error::Error>> {
    let exe_path = std::env::current_exe()?;
    let exe_dir = exe_path.parent().ok_or_else(|| std::io::Error::new(std::io::ErrorKind::NotFound, "无法获取可执行文件目录"))?;
    let config_path = exe_dir.join("config/config.toml"); // 指定到配置文件的正确路径

    let config_str = std::fs::read_to_string(config_path)?;
    let config: Config = toml::from_str(&config_str)?;

    Ok(config)
}