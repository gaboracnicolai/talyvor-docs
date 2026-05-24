import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { searchApi, type SearchResponse, type SearchOptions } from "~/api/search";

// useSearch debounces the query and fans into the unified search
// endpoint. 300ms matches the spec — short enough to feel live,
// long enough that fast typing doesn't fire a request per keystroke.
// Empty / sub-2-char queries short-circuit to no fetch.
export function useSearch(workspaceId: string, opts: SearchOptions = {}) {
  const [query, setQuery] = useState("");
  const [debounced, setDebounced] = useState("");
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (timerRef.current) clearTimeout(timerRef.current);
    timerRef.current = setTimeout(() => setDebounced(query.trim()), 300);
    return () => {
      if (timerRef.current) clearTimeout(timerRef.current);
    };
  }, [query]);

  const result = useQuery<SearchResponse>({
    queryKey: ["search", workspaceId, debounced, opts.type, opts.spaceId, opts.limit],
    queryFn: () => searchApi.search(workspaceId, debounced, opts),
    enabled: debounced.length >= 2,
    staleTime: 30_000,
  });

  return {
    query,
    setQuery,
    debounced,
    data: result.data,
    isLoading: result.isLoading && debounced.length >= 2,
    error: result.error,
  };
}

// Recent-pages persistence — last 10 opened pages, in localStorage so
// it survives reloads. Used as the empty-state of the SearchModal.
const RECENT_KEY = "docs_recent_pages";
const RECENT_LIMIT = 10;

export interface RecentPage {
  page_id: string;
  page_title: string;
  space_name: string;
  url: string;
  opened_at: number;
}

export function getRecentPages(): RecentPage[] {
  try {
    const raw = localStorage.getItem(RECENT_KEY);
    if (!raw) return [];
    const list = JSON.parse(raw) as RecentPage[];
    return Array.isArray(list) ? list : [];
  } catch {
    return [];
  }
}

export function pushRecentPage(p: Omit<RecentPage, "opened_at">) {
  if (!p.page_id) return;
  const next = [
    { ...p, opened_at: Date.now() },
    ...getRecentPages().filter((r) => r.page_id !== p.page_id),
  ].slice(0, RECENT_LIMIT);
  try {
    localStorage.setItem(RECENT_KEY, JSON.stringify(next));
  } catch {
    // Storage quota / Safari private mode — silently drop.
  }
}
