import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { queryString, request } from './client';
import type {
  CalculationDraft,
  CalculationProblem,
  CalculationResult,
  CalculationRule,
  CalculationStatus,
  CalculationValidation,
  CalculationVersion,
  ListParams,
  PageResult,
} from './types';

type UnknownRecord = Record<string, unknown>;

const record = (value: unknown): UnknownRecord => value && typeof value === 'object' ? value as UnknownRecord : {};
const text = (value: unknown): string => typeof value === 'string' ? value : value === undefined || value === null ? '' : String(value);
const number = (value: unknown): number => Number.isFinite(Number(value)) ? Number(value) : 0;
const bool = (value: unknown): boolean => value === true || value === 1 || value === 'true';

function problems(value: unknown): CalculationProblem[] {
  return Array.isArray(value) ? value.map((item) => {
    const source = record(item);
    return {
      ruleIndex: number(source.rule_index) || undefined,
      rule: text(source.rule) || undefined,
      field: text(source.field) || undefined,
      message: text(source.message),
    };
  }) : [];
}

function status(value: unknown): CalculationStatus {
  const source = record(value);
  return {
    routingMode: 'DefaultSources',
    activeRevision: number(source.active_revision),
    configHash: text(source.config_hash),
    ruleCount: number(source.rule_count),
    goodCount: number(source.good_count),
    badCount: number(source.bad_count),
    defaultRouteRevision: number(source.default_route_revision),
    lastEvaluationAt: text(source.last_evaluation_at) || undefined,
    lastEvaluationDurationMs: number(source.last_evaluation_duration_ms),
    lastError: text(source.last_error) || undefined,
    pendingRestart: bool(source.pending_restart),
    pendingRevision: number(source.pending_revision) || undefined,
    pendingHash: text(source.pending_hash) || undefined,
  };
}

function draft(value: unknown): CalculationDraft {
  const source = record(value);
  return {
    revision: number(source.revision), content: text(source.content), hash: text(source.hash), valid: bool(source.valid),
    problems: problems(source.problems), updatedAt: text(source.updated_at) || undefined,
  };
}

function result(value: unknown): CalculationResult {
  const source = record(value);
  return {
    nodeId: text(source.node_id), dataType: text(source.data_type), expression: text(source.expression), value: source.value,
    statusCode: number(source.status_code),
    communicationInputs: Array.isArray(source.communication_inputs) ? source.communication_inputs.map(text).filter(Boolean) : [],
    sourceTimestamp: text(source.source_timestamp) || undefined,
    evaluatedAt: text(source.evaluated_at) || undefined, error: text(source.error) || undefined,
    dependencyCount: number(source.dependency_count), configRevision: number(source.config_revision),
    defaultRouteRevision: number(source.default_route_revision),
  };
}

function rule(value: unknown): CalculationRule {
  const source = record(value);
  return {
    index: number(source.index), kksCalc: text(source.kks_calc), dataType: text(source.data_type), expression: text(source.expression),
    maxLimit: number(source.max_limit), minLimit: number(source.min_limit), defaultValue: number(source.default_value),
    dependencies: Array.isArray(source.dependencies) ? source.dependencies.map(text) : [],
  };
}

function version(value: unknown): CalculationVersion {
  const source = record(value);
  const state = text(source.state);
  return {
    id: number(source.id), revision: number(source.revision), hash: text(source.hash), ruleCount: number(source.rule_count),
    addedCount: number(source.added_count), removedCount: number(source.removed_count), changedCount: number(source.changed_count),
    state: state === 'active' || state === 'pending_restart' ? state : 'archived', actor: text(source.actor),
    createdAt: text(source.created_at) || undefined, activatedAt: text(source.activated_at) || undefined,
  };
}

function page<T>(value: unknown, normalize: (item: unknown) => T): PageResult<T> {
  const source = record(value);
  const limit = Math.max(1, number(source.limit) || 50);
  const offset = Math.max(0, number(source.offset));
  return {
    items: Array.isArray(source.items) ? source.items.map(normalize) : [],
    total: number(source.total), page: Math.floor(offset / limit) + 1, pageSize: limit,
  };
}

export const calculationKeys = {
  all: ['calculations'] as const,
  status: ['calculations', 'status'] as const,
  results: (params: ListParams) => ['calculations', 'results', params] as const,
  allResults: ['calculations', 'results'] as const,
  rules: (params: ListParams) => ['calculations', 'rules', params] as const,
  allRules: ['calculations', 'rules'] as const,
  draft: ['calculations', 'draft'] as const,
  versions: (params: ListParams) => ['calculations', 'versions', params] as const,
  allVersions: ['calculations', 'versions'] as const,
};

export const calculationApi = {
  status: async (): Promise<CalculationStatus> => status(await request('/calculations/status')),
  results: async (params: ListParams, signal?: AbortSignal): Promise<PageResult<CalculationResult>> => page(await request(`/calculations/results${queryString(params)}`, { signal }), result),
  rules: async (params: ListParams, signal?: AbortSignal): Promise<PageResult<CalculationRule>> => page(await request(`/calculations/rules${queryString(params)}`, { signal }), rule),
  draft: async (): Promise<CalculationDraft> => draft(await request('/calculations/config/draft')),
  saveDraft: async (revision: number, content: string): Promise<CalculationDraft> => draft(await request('/calculations/config/draft', { method: 'PUT', headers: { 'If-Match': `"${revision}"` }, body: JSON.stringify({ content }) })),
  importFile: async (revision: number, content: string): Promise<CalculationDraft> => draft(await request('/calculations/config/import-file', { method: 'POST', headers: { 'If-Match': `"${revision}"` }, body: JSON.stringify({ content }) })),
  importLower: async (revision: number, sourceId: string, sourceRevision: number): Promise<CalculationDraft> => draft(await request('/calculations/config/import-lower', { method: 'POST', headers: { 'If-Match': `"${revision}"` }, body: JSON.stringify({ source_id: sourceId, source_revision: sourceRevision }) })),
  validate: async (content?: string): Promise<CalculationValidation> => {
    const source = record(await request('/calculations/config/validate', { method: 'POST', body: JSON.stringify(content === undefined ? {} : { content }) }));
    return { valid: bool(source.valid), hash: text(source.hash), ruleCount: number(source.rule_count), problems: problems(source.problems) };
  },
  activate: async (draftRevision: number): Promise<CalculationVersion> => version(await request('/calculations/config/activate', { method: 'POST', headers: { 'If-Match': `"${draftRevision}"` } })),
  deactivate: async (activeRevision: number): Promise<void> => { await request('/calculations/config/deactivate', { method: 'POST', headers: { 'If-Match': `"${activeRevision}"` } }); },
  versions: async (params: ListParams): Promise<PageResult<CalculationVersion>> => page(await request(`/calculations/config/versions${queryString(params)}`), version),
  restore: async (id: number, activeRevision: number): Promise<CalculationVersion> => version(await request(`/calculations/config/versions/${id}/restore`, { method: 'POST', headers: { 'If-Match': `"${activeRevision}"` } })),
};

export function useCalculationStatus() {
  return useQuery({ queryKey: calculationKeys.status, queryFn: calculationApi.status, refetchInterval: 30_000 });
}

export function useCalculationResults(params: ListParams, enabled = true) {
  return useQuery({ queryKey: calculationKeys.results(params), queryFn: ({ signal }) => calculationApi.results(params, signal), enabled, placeholderData: (previous) => previous });
}

export function useCalculationRules(params: ListParams, enabled = true) {
  return useQuery({ queryKey: calculationKeys.rules(params), queryFn: ({ signal }) => calculationApi.rules(params, signal), enabled, placeholderData: (previous) => previous });
}

export function useCalculationDraft(enabled = true) {
  return useQuery({ queryKey: calculationKeys.draft, queryFn: calculationApi.draft, enabled, staleTime: Number.POSITIVE_INFINITY });
}

export function useCalculationVersions(params: ListParams, enabled = true) {
  return useQuery({ queryKey: calculationKeys.versions(params), queryFn: () => calculationApi.versions(params), enabled });
}

export function useCalculationMutation() {
  const client = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: async (input:
      | { action: 'save' | 'import_file'; revision: number; content: string }
      | { action: 'import_lower'; revision: number; sourceId: string; sourceRevision: number }
      | { action: 'activate'; revision: number }
      | { action: 'deactivate'; revision: number }
      | { action: 'restore'; revision: number; id: number }) => {
      switch (input.action) {
        case 'save': return calculationApi.saveDraft(input.revision, input.content);
        case 'import_file': return calculationApi.importFile(input.revision, input.content);
        case 'import_lower': return calculationApi.importLower(input.revision, input.sourceId, input.sourceRevision);
        case 'activate': return calculationApi.activate(input.revision);
        case 'deactivate': return calculationApi.deactivate(input.revision);
        case 'restore': return calculationApi.restore(input.id, input.revision);
      }
    },
    onSettled: async () => Promise.all([
      client.invalidateQueries({ queryKey: ['calculations', 'state-draft'] }),
      client.invalidateQueries({ queryKey: ['calculations', 'state-inputs'] }),
      client.invalidateQueries({ queryKey: calculationKeys.status }),
      client.invalidateQueries({ queryKey: calculationKeys.draft }),
      client.invalidateQueries({ queryKey: calculationKeys.allVersions }),
      client.invalidateQueries({ queryKey: calculationKeys.allRules }),
      client.invalidateQueries({ queryKey: calculationKeys.allResults }),
      client.invalidateQueries({ queryKey: ['points'] }),
    ]),
  });
}
