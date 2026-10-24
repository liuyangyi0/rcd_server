/** Small EventSource stand-in for lifecycle/invalidations; no timers or samples. */
export class TestEventSource extends EventTarget {
  static instances: TestEventSource[] = [];
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  closed = false;

  constructor(readonly url: string) {
    super();
    TestEventSource.instances.push(this);
  }

  emit(type: string, data: unknown = {}) {
    this.dispatchEvent(new MessageEvent(type, { data: JSON.stringify({ type, data }) }));
  }

  close() { this.closed = true; }
}
