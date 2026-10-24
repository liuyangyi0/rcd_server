import { Link } from 'react-router-dom';

export function RelationshipDiagram() {
  return <figure className="help-relationship" aria-label="软件关系图：数据、日常启停和发布是三条独立链路">
    <figcaption>三条链路，分开理解</figcaption>
    <div className="help-flow">
      <strong>业务数据</strong>
      <div className="help-flow-path"><span>OPC 客户端</span><b aria-label="双向连接">↔</b><Link to="/points">OPC 管理端<small>4840 对客户 / Web 8087</small></Link><b aria-label="双向连接">↔</b><span>Rust 下级<small>采集 4841 / 通信 4842</small></span><b aria-label="双向连接">↔</b><span>串口设备 / 对方机组</span></div>
      <p>读取、订阅和写入走 OPC；Agent 与发布平台不在实时数据路径上。</p>
    </div>
    <div className="help-flow">
      <strong>采集启停</strong>
      <div className="help-flow-path help-flow-short"><Link to="/sources">任一管理网页</Link><b aria-label="调用">→</b><span>目标机器 RCD Agent</span><b aria-label="守护">→</b><span>本机采集进程</span></div>
      <p>Agent 是采集进程唯一守护者；不管理 OPC 管理端，也不代管新通信程序。</p>
    </div>
    <div className="help-flow">
      <strong>软件发布</strong>
      <div className="help-flow-path"><span>Semaphore 网页<small>46:3000</small></span><b aria-label="下发任务">→</b><span>原生任务队列 + Ansible</span><b aria-label="受控执行">→</b><span>受控发布器 / systemd</span><b aria-label="升级">→</b><span>目标软件组件</span></div>
      <p>平台负责升级、恢复和管理端安全启停；业务配置、计算与日志留在管理网页。</p>
    </div>
  </figure>;
}
