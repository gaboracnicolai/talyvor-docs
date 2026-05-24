import { useCallback, useEffect, useRef, useState } from "react";

// Wire-shape types — mirror internal/collab/ot.go exactly so the
// TypeScript compiler is the canary on protocol drift.

export type OpType = "insert" | "delete" | "retain" | "replace";

export interface Op {
  type: OpType;
  pos: number;
  content?: string;
  length?: number;
}

export interface Change {
  id?: string;
  page_id?: string;
  client_id?: string;
  version: number;
  ops: Op[];
  snapshot?: string;
  timestamp?: string;
}

export interface CursorPos {
  from: number;
  to: number;
}

export interface PresenceInfo {
  client_id: string;
  member_id: string;
  member_name: string;
  cursor?: CursorPos;
  color: string;
}

type ServerMessage =
  | { type: "init"; version: number; presence: PresenceInfo[] }
  | { type: "change"; change: Change; version: number }
  | { type: "ack"; id: string; version: number }
  | { type: "cursor"; client_id: string; cursor: CursorPos }
  | { type: "presence"; event: "joined" | "left"; client: PresenceInfo }
  | { type: "pong" }
  | { type: "error"; message: string };

interface UseCollabOptions {
  pageID: string;
  clientID: string;
  memberID: string;
  memberName: string;
  onRemoteChange: (change: Change) => void;
}

interface CollabState {
  connected: boolean;
  version: number;
  presence: PresenceInfo[];
}

// useCollab owns the WebSocket lifecycle for one page. State is
// surface-area only — the consumer (Editor) needs to know who's
// connected and what version the server thinks it's on. The hook
// itself queues outbound changes so a transient disconnect doesn't
// drop edits.
export function useCollab({
  pageID,
  clientID,
  memberID,
  memberName,
  onRemoteChange,
}: UseCollabOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const queueRef = useRef<string[]>([]);
  const reconnectAttempts = useRef(0);
  const [state, setState] = useState<CollabState>({
    connected: false,
    version: 0,
    presence: [],
  });
  const onRemoteChangeRef = useRef(onRemoteChange);
  onRemoteChangeRef.current = onRemoteChange;

  const flushQueue = useCallback(() => {
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    for (const msg of queueRef.current) {
      ws.send(msg);
    }
    queueRef.current = [];
  }, []);

  const connect = useCallback(() => {
    if (!pageID || !clientID) return;
    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    // In dev the Vite proxy forwards /v1/* including /v1/collab/ws.
    // In prod the SPA lives on the same origin as the API, so the
    // relative URL works untouched.
    const host = window.location.host;
    const url = `${proto}//${host}/v1/collab/${pageID}/ws?client_id=${encodeURIComponent(
      clientID,
    )}&member_id=${encodeURIComponent(memberID)}&member_name=${encodeURIComponent(memberName)}`;
    const ws = new WebSocket(url);
    wsRef.current = ws;

    ws.addEventListener("open", () => {
      reconnectAttempts.current = 0;
      setState((s) => ({ ...s, connected: true }));
      flushQueue();
    });

    ws.addEventListener("message", (e) => {
      let msg: ServerMessage;
      try {
        msg = JSON.parse(e.data as string) as ServerMessage;
      } catch {
        return;
      }
      handleMessage(msg);
    });

    ws.addEventListener("close", () => {
      setState((s) => ({ ...s, connected: false }));
      // Exponential backoff: 0.5s, 1s, 2s, 4s, 8s, capped at 30s.
      const attempt = reconnectAttempts.current++;
      const delay = Math.min(30_000, 500 * 2 ** attempt);
      setTimeout(() => connect(), delay);
    });

    ws.addEventListener("error", () => {
      // close fires next; let the reconnect path handle backoff.
    });

    function handleMessage(msg: ServerMessage) {
      switch (msg.type) {
        case "init":
          setState((s) => ({ ...s, version: msg.version, presence: msg.presence ?? [] }));
          break;
        case "change":
          setState((s) => ({ ...s, version: msg.version }));
          onRemoteChangeRef.current(msg.change);
          break;
        case "ack":
          setState((s) => ({ ...s, version: msg.version }));
          break;
        case "cursor":
          setState((s) => ({
            ...s,
            presence: s.presence.map((p) =>
              p.client_id === msg.client_id ? { ...p, cursor: msg.cursor } : p,
            ),
          }));
          break;
        case "presence":
          setState((s) => {
            if (msg.event === "joined") {
              if (s.presence.some((p) => p.client_id === msg.client.client_id)) return s;
              return { ...s, presence: [...s.presence, msg.client] };
            }
            return {
              ...s,
              presence: s.presence.filter((p) => p.client_id !== msg.client.client_id),
            };
          });
          break;
      }
    }
  }, [pageID, clientID, memberID, memberName, flushQueue]);

  useEffect(() => {
    connect();
    return () => {
      const ws = wsRef.current;
      if (ws) {
        // Suppress the reconnect loop on intentional unmount by
        // clearing the ref before closing.
        wsRef.current = null;
        ws.onclose = null;
        ws.close();
      }
    };
  }, [connect]);

  const send = useCallback((payload: unknown) => {
    const msg = JSON.stringify(payload);
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(msg);
    } else {
      queueRef.current.push(msg);
    }
  }, []);

  const sendChange = useCallback(
    (change: Omit<Change, "version" | "client_id" | "page_id"> & { version?: number }) => {
      send({
        type: "change",
        change: {
          ...change,
          version: change.version ?? state.version,
        },
      });
    },
    [send, state.version],
  );

  const sendCursor = useCallback(
    (cursor: CursorPos) => {
      send({ type: "cursor", cursor });
    },
    [send],
  );

  return {
    connected: state.connected,
    version: state.version,
    presence: state.presence,
    sendChange,
    sendCursor,
  };
}
