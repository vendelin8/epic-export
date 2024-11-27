## Epic Export can export your list of games from Epic Games in a nice HTML table.

I tested it on Linux only, but should work on other platforms too.

## Setup
- Install curl, if you don't have it already.

## Usage
1. Log in to epicgames.com
1. Open the browser Developer tools by F12 or Ctrl+Shift+i or equivalent.
1. Open the Network tab on top.
1. Navigate the browser to https://www.epicgames.com/account/connections
1. Switch to the "APPS" tab.
1. In the Developer tools Network tab, seach for "authorized-apps" and click on it.
1. Copy the contents to a file, eg. exported.txt
1. Use that filename with path below in <exported>.
1. Call

On Linux or Mac:

```sh
epic-export -i <exported> -o <output>
```

On Windows:

```sh
epic-export.exe -i <exported> -o <output>
```

It will run through the list of exported games, and search for them.
1. Exact match is stored without prompt.
1. Otherwise it will show a list of matches with some extra options.
  1. You can open the URL on the right to check if you have the game "In Library". Pick it if you're sure about it.
  1. You can ask for logo search. It will initiate a Google Images search by the game logo, and add those at the end of the list.
  1. You can use the game name without the link.
  1. You can skip the game if it was discontinued.
  1. You can type in the game URL by hand of a custom Google Search, maybe based on some text in the logo.

The output will be an html file that shows your games in a table, that you can share with others.

## Run from code
Make sure to [Install Go](https://go.dev/doc/install), if you don't have it already. Then


```sh
go run . -i <exported> -o <output>
```

OR

```sh
go build .
./epic-export -i <exported> -o <output>
```

## Contribute
Feel free to raise an issue or try and build it for other platforms.
