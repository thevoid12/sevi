# Use Cases / Problem Statement

## Current state (prototype v0)

`go run . <url>` fetches the article through Jina Reader, cleans it to speakable
text, chunks it, synthesizes each chunk with Gemini TTS via OpenRouter, and
writes `output/<slug>.mp3`. One URL in, one MP3 out, run by hand from the CLI.
Tested, works well.

## Problem

I need to read a lot of blogs and don't have time to sit and read them. Text is
the wrong medium for when I'm away from a screen. Reading a blog, then asking
questions about it, then reading the answers is text end to end. I want the
whole loop to be audio: listen to the blog, voice a doubt, hear the answer, and
repeat until the concept is clear.

## Use cases

### UC-1: Blog link in, audio out, voice Q&A on the blog

- **Who / when:** me, on the move, listening to blogs instead of reading them.
- **Trigger:** I share a blog URL in a chat surface (Discord channel, Telegram
  bot, or anything else).
- **Flow:**
  1. Share link in the chat surface.
  2. System fetches the blog and converts it to an audio recording (v0 pipeline).
  3. Audio is delivered back. Either posted in the same Discord/Telegram
     channel (cheap, no extra infra), or tracked on a website that stores the
     request and the audio response.
  4. If I don't understand something, I open a thread on that blog. I ask a
     follow-up question by voice (voice note, transcribed to text), the LLM
     answers with the blog as context, and the answer comes back as audio.
  5. I keep going back and forth in the thread until the concept is clear.
- **Expected outcome:** I consume a blog and clear my doubts without reading or
  typing anything.
- **Constraint: pluggable chat layer.** Where the link is shared must be
  customizable. The core (URL to audio, thread Q&A) is independent of the
  surface; Discord, Telegram, web, etc. are thin adapters that others can write
  on top.
- **Open question: interface and threading.** Telegram is attractive because it
  is easy: type or send a voice note, get a reply. Unclear how threads map onto
  it. Possibly two front ends (Telegram and web) over the same core. To be
  decided in the design.
- **Gap in v0:** CLI only, one-shot, no chat input, no delivery, no follow-up
  questions, no history or thread state, no voice input, no LLM answer step.

### UC-2: General voice LLM chat (same idea, no blog)

- **Who / when:** me, same on-the-move situation.
- **Trigger:** a plain LLM conversation, not tied to a blog.
- **Flow:** send questions as voice notes, optionally attach files or other
  material, and get every response back as audio. Continue the conversation
  indefinitely.
- **Expected outcome:** a normal LLM chat that is voice in, voice out, with
  file attachments supported.
- **Relation to UC-1:** the same engine. UC-1's thread Q&A is a chat whose
  context is a blog; UC-2 is a chat with no blog or with attached files as
  context.
- **Gap in v0:** no chat, no speech-to-text, no LLM, no attachments.

## Constraints

- Needs to be usable today for real testing, so v1 must be small.
- Delivery surface is swappable (adapter layer).
- Everything user-facing is audio; text is a fallback, not the primary medium.

## Out of scope

_TBD in design discussion._
