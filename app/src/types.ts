export type Obj = Record<string, unknown>;
export interface Profile {
  // name is the id requests choose the agent by; title the name people read.
  name: string;
  title?: string;
  // main: the main agent ("default"): only its title, description and
  // instructions are its own.
  main?: boolean;
  description?: string;
  models?: string[];
  instructions?: string;
  tools?: string[] | null;
  memory?: string;
}
export interface Conversation {
  id: string;
  title?: string;
  pinned?: boolean;
  project?: string;
  profile?: string;
  updated: string;
  running?: boolean;
}
export interface Call {
  id: string;
  name: string;
  input: unknown;
}
export interface Approval {
  id: string;
  call_id: string;
  tool: string;
  input: unknown;
  worker?: string;
  rule?: string;
  explain?: boolean;
}
export interface Artifact {
  name: string;
  version: number;
  title: string;
  kind: string;
  language?: string;
  media_type: string;
  size: number;
}
export interface Run {
  id: string;
  conversation?: string;
  profile: string;
  state: string;
  text?: string;
  usage?: { cost?: number };
  error?: { code: string; message: string };
  pending?: Approval[];
}
export interface RunEvent {
  id: number;
  kind: string;
  text?: string;
  call?: Call;
  output?: { content: string; is_error?: boolean };
  error?: { message: string };
  ms?: number;
  approval?: Approval;
  decision?: { id: string; allowed: boolean };
  artifact?: Artifact;
  worker?: string;
  parent?: string;
  usage?: { cost?: number };
  state?: string;
}
export interface Message {
  index?: number;
  role: string;
  text?: string;
  calls?: Call[];
  result?: { call_id: string; content: string; is_error?: boolean };
  attachments?: { mime: string; bytes: number }[];
}
export interface Project {
  id: string;
  name: string;
  brief?: string;
  archived?: boolean;
  columns: { name: string; auto?: boolean }[];
  cards?: Card[];
}
export interface Card {
  id: string;
  title: string;
  text?: string;
  column: string;
  agent?: string;
  person?: string;
  conversations?: string[];
}
export interface Timeline {
  seq: number;
  at: string;
  actor: string;
  kind: string;
  text?: string;
  card?: string;
  conversation?: string;
  run?: string;
}
export interface Provider {
  name: string;
  key_env?: string;
  key_set: boolean;
  builtin?: boolean;
}
export interface Settings {
  models: string[];
  providers: Provider[];
  max_cost?: number;
  workflow_cost?: number;
  mcp_servers?: ({ name: string } & Obj)[];
  skills?: { name: string; description?: string }[];
  restart?: string;
  transcribe: Obj;
  speech: Obj;
}
