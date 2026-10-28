export type LisaConfig = {
  apiBasePath?: string;
  headers?: () => HeadersInit | Promise<HeadersInit>;
  fetch?: typeof fetch;
};

export type LisaMessage = {
  id: string;
  role: "user" | "assistant";
  text: string;
  created_at?: string;
  sequence?: number;
  turn_source?: "web" | "slack";
};

export type LisaActivity = {
  id: string;
  kind: "tool" | "status" | "evidence";
  label: string;
  detail?: string;
  session_id?: string;
  created_at?: string;
  sequence?: number;
  tool_call_id?: string;
  tool_name?: string;
  arguments?: string;
  is_error?: boolean;
};

export type LisaConversation = {
  conversation_id: string;
  title: string;
  updated_at?: string;
  event_source?: string;
  event_status?: "pending" | "processing" | "complete" | "failed";
  event_id?: string;
  event_error?: string;
  messages: LisaMessage[];
  activities?: LisaActivity[];
};

export type LisaTurn = {
  conversation_id: string;
  messages: LisaMessage[];
  activities: LisaActivity[];
};

export type LisaFeedEntry = {
  id: string;
  activity_key: string;
  title: string;
  kind: string;
  status: string;
  summary?: string | null;
  subtitle?: string | null;
  status_title?: string | null;
  next_action?: string | null;
  next_action_at?: string | null;
  origin_conversation_id?: string | null;
  origin_slack_channel_id?: string | null;
  origin_slack_thread_ts?: string | null;
  created_at: string;
  updated_at: string;
  finished_at?: string | null;
};

export type LisaActivityDetail = {
  activity: LisaFeedEntry;
  thread: { activity_thread_id: string; name: string; status: string; status_title?: string | null; details_json?: Record<string, unknown>; finish_message?: string | null };
  actions: Array<{ seq: number; event_type: string; title: string; summary?: string | null; actor?: string; reference_type?: string | null; reference_id?: string | null; status?: string; created_at: string }>;
  sections: Array<{ activity_thread_id: string; section_key: string; title: string; status: string; section_order: number; section_depth: number; parent_section_key?: string | null; ordinal_label?: string | null }>;
  references: Array<{ reference_type: string; reference_id: string }>;
};

export type LisaPolicyRecord = {
  id: string;
  scope: string;
  target_agent_name?: string | null;
  name: string;
  criteria: string;
  approved: boolean;
  updated_at?: string | null;
};

export type LisaPolicyPage = {
  items: LisaPolicyRecord[];
  next_offset: number | null;
};

export type LisaPolicySuggestionEvidence = {
  signal_id?: string;
  session_id: string;
  activity_id: string;
  agent_name: string;
  summary?: string;
  span_id: string;
  kind: string;
  name?: string | null;
  content?: string | null;
  payload?: unknown;
  status?: string | null;
};

export type LisaPolicySuggestion = {
  id: string;
  name: string;
  criteria: string;
  scope: "workspace" | "agent";
  target_agent_name: string;
  rationale: string;
  signal_count: number;
  evidence: Array<{
    signal_id: string;
    session_id: string;
    activity_id: string;
    agent_name: string;
    summary: string;
    span_ids: string[];
    evidence: LisaPolicySuggestionEvidence[];
  }>;
  overlaps: Array<{ policy_id: string; relation: string; rationale: string }>;
  status: "pending" | "approved" | "dismissed";
  policy_id?: string | null;
  created_at: string;
  reviewed_at?: string | null;
};

export type LisaPolicySuggestionPage = {
  items: LisaPolicySuggestion[];
  can_review: boolean;
};

export type LisaProactiveUpdate = {
  id: string;
  source_type: "policy_suggestion" | "policy_finding" | "qa_observation";
  source_id: string;
  messages: string[];
  activity_id?: string | null;
  created_at: string;
};

export type LisaQaObservation = {
  id: string;
  session_id: string;
  activity_id: string;
  agent_name: string;
  summary: string;
  span_ids: string[];
  evidence: Array<{
    span_id: string;
    kind: string;
    name?: string | null;
    content?: string | null;
    payload?: unknown;
    status?: string | null;
  }>;
  created_at: string;
};

export class LisaApi {
  constructor(readonly config: LisaConfig = {}) {}

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const base = (this.config.apiBasePath ?? "/v1").replace(/\/$/, "");
    const authHeaders = await this.config.headers?.();
    const response = await (this.config.fetch ?? fetch)(`${base}${path}`, {
      ...init,
      headers: { Accept: "application/json", ...authHeaders, ...init.headers },
    });
    if (!response.ok) throw new Error(`${init.method ?? "GET"} ${base}${path}: HTTP ${response.status}`);
    return response.json() as Promise<T>;
  }

  listLisaConversations(): Promise<LisaConversation[]> {
    return this.request("/conversations");
  }

  createLisaConversation(): Promise<LisaConversation> {
    return this.request("/conversations", { method: "POST" });
  }

  getLisaConversation(conversationId: string): Promise<LisaConversation> {
    return this.request(`/conversations/${encodeURIComponent(conversationId)}`);
  }

  deleteLisaConversation(conversationId: string): Promise<{ deleted: boolean }> {
    return this.request(`/conversations/${encodeURIComponent(conversationId)}`, { method: "DELETE" });
  }

  sendLisaMessage(conversationId: string, text: string): Promise<LisaTurn> {
    return this.request(`/conversations/${encodeURIComponent(conversationId)}/messages`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      // The composer displays the provider/data disclosure next to Send.
      // Sending a message is the user's affirmative action to proceed.
      body: JSON.stringify({ text, consent: true }),
    });
  }

  listLisaActivities(params: { limit?: number; activityType?: string; status?: string; query?: string; openOnly?: boolean; dueOnly?: boolean } = {}): Promise<LisaFeedEntry[]> {
    const query = new URLSearchParams();
    if (params.limit != null) query.set("limit", String(params.limit));
    if (params.activityType) query.set("activity_type", params.activityType);
    if (params.status) query.set("status", params.status);
    if (params.query) query.set("query", params.query);
    if (params.openOnly) query.set("open_only", "true");
    if (params.dueOnly) query.set("due_only", "true");
    return this.request(`/activities?${query.toString()}`);
  }

  listLisaPolicies(params: { limit?: number; offset?: number } = {}): Promise<LisaPolicyPage> {
    const query = new URLSearchParams();
    if (params.limit != null) query.set("limit", String(params.limit));
    if (params.offset != null) query.set("offset", String(params.offset));
    return this.request(`/policies?${query.toString()}`);
  }

  listLisaPolicySuggestions(params: { status?: string; limit?: number } = {}): Promise<LisaPolicySuggestionPage> {
    const query = new URLSearchParams();
    if (params.status) query.set("status", params.status);
    if (params.limit != null) query.set("limit", String(params.limit));
    return this.request(`/policy-suggestions?${query.toString()}`);
  }

  listLisaProactiveUpdates(limit = 50): Promise<{ items: LisaProactiveUpdate[] }> {
    return this.request(`/proactive-updates?limit=${encodeURIComponent(String(limit))}`);
  }

  getLisaQaObservation(observationId: string): Promise<LisaQaObservation> {
    return this.request(`/qa-observations/${encodeURIComponent(observationId)}`);
  }

  approveLisaPolicySuggestion(
    suggestionId: string,
    body: { name?: string; criteria?: string } = {},
  ): Promise<{ suggestion: LisaPolicySuggestion; policy: LisaPolicyRecord }> {
    return this.request(`/policy-suggestions/${encodeURIComponent(suggestionId)}/approve`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
  }

  dismissLisaPolicySuggestion(suggestionId: string): Promise<LisaPolicySuggestion> {
    return this.request(`/policy-suggestions/${encodeURIComponent(suggestionId)}/dismiss`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({}),
    });
  }

  getLisaActivity(activityId: string): Promise<LisaActivityDetail> {
    return this.request(`/activities/${encodeURIComponent(activityId)}`);
  }


}
