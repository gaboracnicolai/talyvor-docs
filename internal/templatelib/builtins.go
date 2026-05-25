package templatelib

import (
	"sync"

	"github.com/talyvor/docs/internal/mcp"
)

// Builtin templates ship in code. Twenty starter docs cover the
// most-requested team rituals: engineering RFC/ADR/post-mortem,
// product PRD/roadmap/research, HR onboarding/JD/review,
// marketing launch/email/calendar, and a small core of operations
// + general docs. Storing them in code rather than a DB seed means
// the gallery is functional even before the first migration runs,
// and shipping a fresh template is a one-line change.

// builtinSeed pairs a built-in's metadata with the markdown body
// the gallery converts to ProseMirror on first load.
type builtinSeed struct {
	id          string
	name        string
	description string
	category    TemplateCategory
	icon        string
	tags        []string
	markdown    string
}

var builtinSeeds = []builtinSeed{
	// ─── Engineering ───
	{
		id:          "builtin-rfc",
		name:        "RFC (Request for Comments)",
		description: "Propose a design change and gather feedback before building.",
		category:    CatEngineering,
		icon:        "📋",
		tags:        []string{"rfc", "design", "proposal"},
		markdown: `# RFC: Title

## Summary

## Motivation

## Detailed Design

## Drawbacks

## Alternatives

## Adoption Strategy

## Unresolved Questions`,
	},
	{
		id:          "builtin-postmortem",
		name:        "Incident Post-Mortem",
		description: "Blameless write-up of an incident with timeline and action items.",
		category:    CatEngineering,
		icon:        "🐛",
		tags:        []string{"incident", "postmortem", "ops"},
		markdown: `# Incident Post-Mortem

## Incident Summary

## Timeline

## Root Cause

## Impact

## Resolution

## Action Items

- [ ] `,
	},
	{
		id:          "builtin-adr",
		name:        "Architecture Decision Record (ADR)",
		description: "Capture a single architectural decision and the reasons behind it.",
		category:    CatEngineering,
		icon:        "🏗️",
		tags:        []string{"adr", "architecture", "decision"},
		markdown: `# ADR: Title

## Status

## Context

## Decision

## Consequences

## Alternatives Considered`,
	},
	{
		id:          "builtin-api",
		name:        "API Documentation",
		description: "Reference doc for an API surface, endpoints, and error codes.",
		category:    CatEngineering,
		icon:        "🔌",
		tags:        []string{"api", "reference"},
		markdown: `# API: Service Name

## Overview

## Authentication

## Endpoints

### GET /endpoint

**Parameters**

**Response**

## Error Codes

## Rate Limits

## Changelog`,
	},
	{
		id:          "builtin-runbook",
		name:        "Deployment Runbook",
		description: "Step-by-step deployment + rollback procedure.",
		category:    CatEngineering,
		icon:        "🚀",
		tags:        []string{"deploy", "runbook"},
		markdown: `# Deployment Runbook

## Prerequisites

## Pre-deployment Checklist

- [ ]

## Deployment Steps

## Rollback Procedure

## Monitoring`,
	},
	{
		id:          "builtin-techspec",
		name:        "Technical Specification",
		description: "End-to-end design for a feature or system.",
		category:    CatEngineering,
		icon:        "✅",
		tags:        []string{"spec", "design"},
		markdown: `# Technical Specification

## Problem Statement

## Goals

## Non-goals

## Proposed Solution

## Implementation Plan

## Testing Strategy

## Open Questions`,
	},

	// ─── Product ───
	{
		id:          "builtin-prd",
		name:        "Product Requirements Document (PRD)",
		description: "Outline what's being built, for whom, and why.",
		category:    CatProduct,
		icon:        "📝",
		tags:        []string{"prd", "requirements"},
		markdown: `# PRD: Feature Name

## Overview

## Problem

## Goals

## User Stories

## Acceptance Criteria

## Out of Scope

## Timeline`,
	},
	{
		id:          "builtin-roadmap",
		name:        "Product Roadmap",
		description: "Quarterly plan with success metrics.",
		category:    CatProduct,
		icon:        "🗺️",
		tags:        []string{"roadmap", "planning"},
		markdown: `# Product Roadmap

## Vision

## Q1 Goals

## Q2 Goals

## Q3 Goals

## Q4 Goals

## Success Metrics`,
	},
	{
		id:          "builtin-research",
		name:        "User Research Notes",
		description: "Capture findings from user interviews or research sessions.",
		category:    CatProduct,
		icon:        "🔬",
		tags:        []string{"research", "interviews"},
		markdown: `# User Research

## Research Questions

## Methodology

## Participants

## Key Findings

## Recommendations

## Next Steps`,
	},
	{
		id:          "builtin-product-review",
		name:        "Product Review",
		description: "Retro on what shipped, with metrics and learnings.",
		category:    CatProduct,
		icon:        "📊",
		tags:        []string{"review", "retro"},
		markdown: `# Product Review

## What We Shipped

## Metrics

## What Went Well

## What Didn't

## Learnings

## Next Sprint Focus`,
	},

	// ─── HR ───
	{
		id:          "builtin-onboarding",
		name:        "Employee Onboarding",
		description: "Welcome doc with a first-week checklist + 30/60/90 goals.",
		category:    CatHR,
		icon:        "👋",
		tags:        []string{"onboarding", "hr"},
		markdown: `# Welcome!

## Week 1 Checklist

- [ ] Set up tools
- [ ] Meet your team

## Company Overview

## Your Role

## 30/60/90 Day Goals`,
	},
	{
		id:          "builtin-jd",
		name:        "Job Description",
		description: "Role overview, requirements, and what we offer.",
		category:    CatHR,
		icon:        "📋",
		tags:        []string{"job", "hiring"},
		markdown: `# Job Description: Role

## Role Overview

## Responsibilities

## Requirements

## Nice to Have

## What We Offer`,
	},
	{
		id:          "builtin-perf-review",
		name:        "Performance Review",
		description: "Periodic check-in covering goals, growth, and next steps.",
		category:    CatHR,
		icon:        "🎯",
		tags:        []string{"review", "growth"},
		markdown: `# Performance Review

## Goals Review

## Accomplishments

## Areas for Growth

## Goals for Next Period

## Manager Notes`,
	},

	// ─── Marketing ───
	{
		id:          "builtin-launch",
		name:        "Launch Plan",
		description: "Coordinate a launch across audience, channels, and timeline.",
		category:    CatMarketing,
		icon:        "📣",
		tags:        []string{"launch", "marketing"},
		markdown: `# Launch Plan

## Launch Overview

## Target Audience

## Key Messages

## Channels

## Timeline

## Success Metrics`,
	},
	{
		id:          "builtin-email-brief",
		name:        "Email Campaign Brief",
		description: "Single-page brief for an email campaign.",
		category:    CatMarketing,
		icon:        "📧",
		tags:        []string{"email", "campaign"},
		markdown: `# Email Campaign Brief

## Campaign Goal

## Audience

## Subject Lines

## Key CTA

## Schedule

## Success Metrics`,
	},
	{
		id:          "builtin-content-calendar",
		name:        "Content Calendar",
		description: "Editorial calendar with status tracking. Add /database for a structured view.",
		category:    CatMarketing,
		icon:        "📱",
		tags:        []string{"content", "calendar"},
		markdown: `# Content Calendar

Track upcoming content across channels. Use ` + "`/database`" + ` to add a table with columns:

- **Date** (date)
- **Title** (text)
- **Channel** (select: Blog, Social, Email, Podcast)
- **Status** (select: Idea, Drafting, Reviewing, Published)
- **Author** (text)`,
	},

	// ─── Operations ───
	{
		id:          "builtin-meeting-notes",
		name:        "Meeting Notes",
		description: "Standard meeting template with attendees, decisions, and action items.",
		category:    CatOperations,
		icon:        "📅",
		tags:        []string{"meeting", "notes"},
		markdown: `# Meeting Notes

## Date:

## Attendees:

## Agenda

1.

## Decisions Made

## Action Items

- [ ] Owner: Due:`,
	},
	{
		id:          "builtin-process",
		name:        "Process Documentation",
		description: "Document a repeatable process with steps + exceptions.",
		category:    CatOperations,
		icon:        "🔄",
		tags:        []string{"process", "sop"},
		markdown: `# Process: Name

## Purpose

## When to Use This Process

## Steps

1.

## Exceptions

## Related Documents`,
	},

	// ─── General ───
	{
		id:          "builtin-project-overview",
		name:        "Project Overview",
		description: "Catch-all project landing page.",
		category:    CatGeneral,
		icon:        "📓",
		tags:        []string{"project", "summary"},
		markdown: `# Project Overview

## Project Summary

## Goals

## Team

## Timeline

## Key Links

## Status Updates`,
	},
	{
		id:          "builtin-weekly-update",
		name:        "Weekly Update",
		description: "Lightweight team update covering progress + blockers.",
		category:    CatGeneral,
		icon:        "✍️",
		tags:        []string{"update", "status"},
		markdown: `# Weekly Update

## This Week

## Next Week

## Blockers

## Links`,
	},
}

// BuiltinCount lets tests assert on the registered count without
// importing the slice's length expression. Updated whenever the
// gallery ships new built-ins.
var BuiltinCount = len(builtinSeeds)

// builtinsOnce + builtinsRendered hydrates the seed list into the
// LibraryTemplate shape on first call. We render markdown lazily so
// the package import cost stays low; once hydrated the slice is
// cached for the process lifetime.
var (
	builtinsOnce     sync.Once
	builtinsRendered []LibraryTemplate
)

// Builtins returns the rendered built-in templates. Always returns
// the same slice (rendered on first call), with deterministic order
// matching builtinSeeds.
func Builtins() []LibraryTemplate {
	builtinsOnce.Do(func() {
		builtinsRendered = make([]LibraryTemplate, 0, len(builtinSeeds))
		for _, s := range builtinSeeds {
			builtinsRendered = append(builtinsRendered, LibraryTemplate{
				ID:          s.id,
				Name:        s.name,
				Description: s.description,
				Category:    s.category,
				Icon:        s.icon,
				Tags:        s.tags,
				Content:     mcp.MarkdownToProseMirror(s.markdown),
				ContentText: s.markdown,
				IsBuiltIn:   true,
			})
		}
	})
	out := make([]LibraryTemplate, len(builtinsRendered))
	copy(out, builtinsRendered)
	return out
}

// builtinByID looks up a built-in by its stable ID, returning nil
// when the ID isn't a built-in.
func builtinByID(id string) *LibraryTemplate {
	for i := range builtinsRendered {
		if builtinsRendered[i].ID == id {
			out := builtinsRendered[i]
			return &out
		}
	}
	// Render on demand if Builtins() hasn't been called yet.
	_ = Builtins()
	for i := range builtinsRendered {
		if builtinsRendered[i].ID == id {
			out := builtinsRendered[i]
			return &out
		}
	}
	return nil
}
