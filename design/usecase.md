# Use Cases / Problem Statement

## Where we started (the CLI prototype)

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
  2. System fetches the blog and converts it to an audio recording (the prototype's pipeline).
  3. Audio is delivered back. Either posted in the same Discord/Telegram
     channel (cheap, no extra infra), or tracked on a website that stores the
     request and the audio response.
  4. If I don't understand something, I reply to a message (the blog audio,
     one of my earlier voice notes, or one of the bot's answers) with a
     follow-up question by voice or text. The LLM answers with the blog and
     that thread of messages as context.
  5. I keep going back and forth until the concept is clear. I can reply to
     any earlier message, not just the latest one; it all stays one thread.
- **One thread per blog.** All questions about a blog, whichever message I
  reply to, form a single conversation. The model sees the whole blog
  conversation in time order, each message with when it was sent, so it
  understands what happened and when. The message I reply to tells the model
  which one I am referring to ("replying to: ...").
- **Reply format.** Answers come back as a voice note by default (a config
  setting, on by default). If voice is switched off, answers are text. Even
  with voice on, if I ask in the message for a text answer ("answer in text",
  "write it out"), I get text for that turn. The blog's own audio is always
  audio.
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
- **Gap in the prototype:** CLI only, one-shot, no chat input, no delivery, no follow-up
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
- **Gap in the prototype:** no chat, no speech-to-text, no LLM, no attachments.
- **Status:** voice and text chat is built (version 0). File attachments are version 1.

## Constraints

- Needs to be usable today for real testing, so the first version must be small.
- Delivery surface is swappable (adapter layer).
- Everything user-facing is audio; text is a fallback, not the primary medium.

## Out of scope

_TBD in design discussion._
