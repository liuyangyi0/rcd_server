import { useEffect, useRef } from 'react';
import { Link, useLocation, useSearchParams } from 'react-router-dom';
import { ArrowRight, BookOpen, Search } from 'lucide-react';
import { PageHeader } from '../components/PageHeader';
import { CopyTextButton } from '../components/CopyTextButton';
import { findHelpTopics, helpCategories, helpTopics, type HelpSection, type HelpTopic } from '../features/help/content';
import { RelationshipDiagram } from '../features/help/RelationshipDiagram';
import { helpScreenshots } from '../features/help/screenshots';
import './HelpPage.css';

export default function HelpPage() {
  const [params, setParams] = useSearchParams();
  const location = useLocation();
  const query = params.get('q') ?? '';
  const category = params.get('category') ?? '';
  const topicId = params.get('topic') ?? 'relationships';
  const topic = helpTopics.find((item) => item.id === topicId);
  const results = findHelpTopics(query, category);
  const filtering = Boolean(query.trim() || category);
  const title = useRef<HTMLHeadingElement>(null);
  const focusArticle = useRef(false);
  useEffect(() => {
    // Router navigation can commit after the click. Do not consume the focus
    // request while search results (and no article heading) are still mounted.
    if (!filtering && focusArticle.current && title.current) {
      title.current.focus(); focusArticle.current = false;
    }
  }, [location.key, topicId, filtering]);

  function filter(key: string, value: string) {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value); else next.delete(key);
    setParams(next, { replace: true });
  }

  return <div className="page help-page">
    <PageHeader title="帮助中心" description="现有软件的图文使用说明：真实页面截图、操作步骤及可复制 OPC 示例，不重做业务页面或发布平台。" />
    <div className="help-toolbar" role="search" aria-label="搜索帮助">
      <label className="help-search"><Search size={17} aria-hidden="true" /><span className="help-sr-only">搜索说明、节点或参数</span><input type="search" value={query} placeholder="搜索说明、NodeId、参数，例如：发送间隔" onChange={(event) => filter('q', event.target.value)} /></label>
      <label className="help-category">主题分类<select value={category} onChange={(event) => filter('category', event.target.value)}><option value="">全部分类</option>{helpCategories.map((item) => <option key={item}>{item}</option>)}</select></label>
      {filtering ? <button className="button button-outline" type="button" onClick={() => setParams({ topic: topic?.id ?? 'relationships' })}>清除筛选</button> : null}
    </div>
    <div className="help-layout">
      <nav className="help-topics" aria-label="帮助主题">
        <span className="help-topics-label"><BookOpen size={16} aria-hidden="true" />使用指南</span>
        {helpCategories.map((item) => <div key={item}><h2>{item}</h2>{helpTopics.filter((entry) => entry.category === item).map((entry) => <Link key={entry.id} to={`/help?topic=${entry.id}`} aria-current={!filtering && topic?.id === entry.id ? 'page' : undefined} onClick={() => { focusArticle.current = true; }}>{entry.title}</Link>)}</div>)}
      </nav>
      <div className="help-content">
        {filtering ? <section className="help-results" aria-labelledby="help-results-title">
          <h2 id="help-results-title">找到 {results.length} 个主题</h2>
          <p role="status">{results.length ? '点击主题查看完整步骤与可复制示例。' : '未找到匹配内容。请减少关键词或清除分类筛选。'}</p>
          {results.map((entry) => <Link className="help-result" key={entry.id} to={`/help?topic=${entry.id}`} onClick={() => { focusArticle.current = true; }}><small>{entry.category}</small><strong>{entry.title}<ArrowRight size={16} aria-hidden="true" /></strong><span>{entry.summary}</span></Link>)}
        </section> : topic ? <article className="help-article" aria-labelledby="help-article-title" key={topic.id}>
          <header><small>{topic.category}</small><h2 id="help-article-title" tabIndex={-1} ref={title}>{topic.title}</h2><p>{topic.summary}</p></header>
          {topic.id === 'relationships' ? <RelationshipDiagram /> : null}
          <nav className="help-toc" aria-label="本篇目录"><span>本篇内容</span>{topic.sections.map((section, index) => <button type="button" key={section.title} onClick={() => { const heading = document.getElementById(`help-section-${index}`); heading?.scrollIntoView({ behavior: 'auto', block: 'start' }); heading?.focus({ preventScroll: true }); }}>{section.title}</button>)}</nav>
          {topic.sections.map((section, index) => <HelpSectionView key={section.title} section={section} index={index} topicId={topic.id} />)}
          <RelatedLinks topic={topic} />
          <footer className="help-readonly">本页只提供说明与复制，不执行设备命令、不修改配置、不启停服务。示例中的占位符及现场数据须自行替换。</footer>
        </article> : <section className="help-missing"><h2>未找到这个帮助主题</h2><p>链接可能已失效，其他功能不受影响。</p><Link className="button button-outline" to="/help">返回软件关系与入口</Link></section>}
      </div>
    </div>
  </div>;
}

function HelpSectionView({ section, index, topicId }: { section: HelpSection; index: number; topicId: string }) {
  return <section className="help-section" aria-labelledby={`help-section-${index}`}>
    <h3 id={`help-section-${index}`} tabIndex={-1}>{section.title}</h3>
    {section.paragraphs?.map((text) => <p key={text}>{text}</p>)}
    {section.steps ? <ol className="help-steps">{section.steps.map((text) => <li key={text}>{text}</li>)}</ol> : null}
    {(helpScreenshots[`${topicId}:${index}`] ?? []).map((shot) => <figure className="help-screenshot" key={shot.id}>
      <figcaption><strong>{shot.caption}</strong><span>真实页面 · {new Date(shot.capturedAt).toLocaleDateString('zh-CN', { timeZone: 'Asia/Shanghai' })} · 当时快照，非实时状态</span></figcaption>
      <a href={`${import.meta.env.BASE_URL}help/screenshots/${shot.id}.jpg`} target="_blank" rel="noreferrer" aria-label={`查看原图：${shot.caption}`}><img src={`${import.meta.env.BASE_URL}help/screenshots/${shot.id}.jpg`} alt={shot.caption} width={1440} height={960} loading="lazy" /></a>
      <ol>{shot.points.map((point) => <li key={point}>{point}</li>)}</ol>
      <a className="help-image-link" href={`${import.meta.env.BASE_URL}help/screenshots/${shot.id}.jpg`} target="_blank" rel="noreferrer">查看原图（新标签页）</a>
    </figure>)}
    {section.caution ? <aside className="help-caution"><strong>注意</strong><p>{section.caution}</p></aside> : null}
    {section.nodes ? <div className="help-nodes">{section.nodes.map((node) => <div className="help-node" key={node.name}><div><code>{node.name}</code><CopyTextButton value={node.name} label="复制节点" /></div><span className="help-node-meta">{node.type} · {node.access}</span><p>{node.description}</p></div>)}</div> : null}
    {section.table ? <div className="help-table-scroll" role="region" aria-label={`${section.title}表格`} tabIndex={0}><table className="help-table"><thead><tr>{section.table.headings.map((heading) => <th key={heading} scope="col">{heading}</th>)}</tr></thead><tbody>{section.table.rows.map((row, rowIndex) => <tr key={rowIndex}>{row.map((cell, col) => <td key={col}>{cell}</td>)}</tr>)}</tbody></table></div> : null}
    {section.examples?.map((example) => <div className="help-example" key={example.title}><div className="help-example-header"><h4>{example.title}</h4><CopyTextButton value={example.text} label={`复制${example.title}`} /></div>{example.note ? <p>{example.note}</p> : null}<pre tabIndex={0} aria-label={example.title}><code>{example.text}</code></pre></div>)}
  </section>;
}

function RelatedLinks({ topic }: { topic: HelpTopic }) {
  return <nav className="help-related" aria-label="前往操作页面"><strong>去哪里操作</strong>{topic.links.map((link) => <Link className="button button-outline" key={link.to} to={link.to}>{link.label}<ArrowRight size={15} aria-hidden="true" /></Link>)}</nav>;
}
