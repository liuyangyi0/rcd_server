import { accessSync, readFileSync } from 'node:fs';
import { URL as NodeURL } from 'node:url';
import { describe, expect, it } from 'vitest';
import { atomicExample, findHelpTopics, helpTopics, transmitExampleFields } from './content';
import { helpScreenshots } from './screenshots';

describe('bundled help contracts', () => {
  it('covers every software and maintenance responsibility without adding execution links', () => {
    const all = JSON.stringify(helpTopics);
    for (const name of ['Semaphore', 'Ansible', 'RCD Agent', '串口通信程序', '采集程序', 'OPC 管理端']) expect(all).toContain(name);
    const platform = helpTopics.find((topic) => topic.id === 'platform')!;
    expect(platform.category).toBe('软件维护升级');
    for (const phrase of ['上传软件包', '首次安装', '恢复', '命令记录', '人为停止', 'command_interval_ms']) expect(all).toContain(phrase);
    expect(new Set(helpTopics.map((topic) => topic.id)).size).toBe(helpTopics.length);
    for (const topic of helpTopics) for (const link of topic.links) expect(link.to).toMatch(/^\/(settings|sources|points|groups|calculations|commands|device-versions|lower-logs|operations)$/);
  });

  it('matches the complete original receive/send node names and types in the client guide', () => {
    const guide = readFileSync(new NodeURL('../../../../../communicate/docs/opc-client-guide.md', import.meta.url), 'utf8');
    const topic = helpTopics.find((item) => item.id === 'communication')!;
    for (const [from, to, title, count] of [
      ['## 3.', '## 4.', '接收点：只读，共 15 项', 15],
      ['## 4.', '## 5.', '发送输入：读写，共 39 项', 39],
    ] as const) {
      const part = guide.split(from)[1].split(to)[0];
      const expected = [...part.matchAll(/^\| \d+ \| `([^`]+)` \| (UInt32|String) \|/gm)].map((match) => [match[1], match[2]]);
      const actual = topic.sections.find((section) => section.title === title)!;
      expect(expected).toHaveLength(count);
      expect('nodes' in actual && actual.nodes.map((node) => [node.name.replace('ns=2;s=', ''), node.type])).toEqual(expected);
    }
    const original = guide.match(/### 7\.2[\s\S]*?```json\s+([\s\S]*?)```/);
    expect(original).not.toBeNull();
    const fields = JSON.parse(original![1]).fields;
    expect(Object.keys(transmitExampleFields).sort()).toEqual(Object.keys(fields).sort());
  });

  it('keeps the atomic example complete, typed and separate from session selection revision', () => {
    const value = JSON.parse(atomicExample);
    expect(Object.keys(value)).toEqual(['revision', 'fields']);
    expect(value.revision).toBe(0);
    expect(Object.keys(value.fields)).toHaveLength(39);
    expect(value.fields['DATA.AKONs.1CYE18GH204.HW']).toMatch(/^[0-9A-F]{1938}$/);
    expect(value.fields['DATA.PumpStationCtrl']).toMatch(/^[0-9A-F]{16}$/);
    expect(value.fields).not.toHaveProperty('DATA.Header.nBlock.z');
    for (const [name, field] of Object.entries(value.fields)) if (name.endsWith('.Fire')) expect(field).toBe(1);
    const selection = JSON.stringify(helpTopics.find((topic) => topic.id === 'selection'));
    expect(selection).toContain('UInt64');
    expect(selection).toContain('BadInvalidState');
    expect(selection).toContain('不是发送快照 revision');
  });

  it('searches literal terms in nodes, content and examples with category intersection', () => {
    expect(findHelpTopics('TRANSMITSNAPSHOT').map((topic) => topic.id)).toContain('communication');
    expect(findHelpTopics('command_interval_ms').map((topic) => topic.id)).toContain('commands');
    expect(findHelpTopics('上传 软件包', '软件维护升级').map((topic) => topic.id)).toEqual(['platform']);
    expect(findHelpTopics('DATA.Buildings.91UBH.Fire').length).toBeGreaterThan(0);
    expect(findHelpTopics('[').length).toBeGreaterThan(0);
    expect(findHelpTopics('<script>不存在</script>')).toEqual([]);
    expect(findHelpTopics('', '不存在的分类')).toEqual([]);
  });

  it('ships every referenced real screenshot, with dates, captions and matching chapter locations', () => {
    for (const [location, shots] of Object.entries(helpScreenshots)) {
      const [id, index] = location.split(':');
      const topic = helpTopics.find((entry) => entry.id === id)!;
      expect(topic.sections[Number(index)]).toBeDefined();
      for (const shot of shots) {
        expect(shot.id).toMatch(/^(manager|platform)-[a-z-]+$/);
        expect(Number.isFinite(Date.parse(shot.capturedAt))).toBe(true);
        expect(shot.points.length).toBeGreaterThan(0);
        const image = new NodeURL(`../../../public/help/screenshots/${shot.id}.jpg`, import.meta.url);
        expect(() => accessSync(image)).not.toThrow();
        expect(readFileSync(image).subarray(0, 3).toString('hex')).toBe('ffd8ff');
      }
    }
  });
});
