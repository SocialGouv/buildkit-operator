import { getParserRegexes } from './regex.js';
import { trimNewLines, appendLine, getCommentFilter, gpgFilter, truncateToScissor, assignMatchedCorrespondence } from './utils.js';
import { defaultOptions } from './options.js';
/**
 * Helper to create commit object.
 * @param initialData - Initial commit data.
 * @returns Commit object with empty data.
 */
export function createCommitObject(initialData = {}) {
    // @ts-expect-error: You can read properties from `Commit` without problems, but you can't assign object to this type. So here is helper for that.
    return {
        merge: null,
        revert: null,
        header: null,
        body: null,
        footer: null,
        notes: [],
        mentions: [],
        references: [],
        ...initialData
    };
}
/**
 * Commit message parser.
 */
export class CommitParser {
    options;
    regexes;
    lines = [];
    lineIndex = 0;
    commit = createCommitObject();
    constructor(options = {}) {
        this.options = {
            ...defaultOptions,
            ...options
        };
        this.regexes = getParserRegexes(this.options);
    }
    currentLine() {
        return this.lines[this.lineIndex];
    }
    nextLine() {
        return this.lines[this.lineIndex++];
    }
    isLineAvailable() {
        return this.lineIndex < this.lines.length;
    }
    parseReference(input, action) {
        const { regexes } = this;
        if (regexes.url.test(input)) {
            return null;
        }
        const matches = regexes.referenceParts.exec(input);
        if (!matches) {
            return null;
        }
        let [raw, repository = null, prefix, issue] = matches;
        let owner = null;
        if (repository) {
            const slashIndex = repository.indexOf('/');
            if (slashIndex !== -1) {
                owner = repository.slice(0, slashIndex);
                repository = repository.slice(slashIndex + 1);
            }
        }
        return {
            raw,
            action,
            owner,
            repository,
            prefix,
            issue
        };
    }
    parseReferences(input, isFooterToken = false) {
        const { regexes } = this;
        const regex = isFooterToken
            ? regexes.footerReferences
            : regexes.references;
        // Text outside of action sentences, like `#1` in `#1 Fixes #2`,
        // may also have references, but without an action
        const sentences = [];
        let lastIndex = 0;
        let matches;
        while (true) {
            matches = regex.exec(input);
            if (!matches) {
                break;
            }
            sentences.push([null, input.slice(lastIndex, matches.index)], [matches[1] || null, matches[2] || '']);
            lastIndex = regex.lastIndex;
        }
        sentences.push([null, input.slice(lastIndex)]);
        const references = [];
        let reference;
        for (const [action, sentence] of sentences) {
            while (true) {
                reference = this.parseReference(sentence, action);
                if (!reference) {
                    break;
                }
                references.push(reference);
            }
        }
        return references;
    }
    skipEmptyLines() {
        let line = this.currentLine();
        while (line !== undefined && !line.trim()) {
            this.nextLine();
            line = this.currentLine();
        }
    }
    parseMerge() {
        const { commit, options } = this;
        const correspondence = options.mergeCorrespondence || [];
        const merge = this.currentLine();
        const matches = merge && options.mergePattern
            ? merge.match(options.mergePattern)
            : null;
        if (matches) {
            this.nextLine();
            commit.merge = matches[0] || null;
            assignMatchedCorrespondence(commit, matches, correspondence);
            return true;
        }
        return false;
    }
    parseHeader(isMergeCommit) {
        if (isMergeCommit) {
            this.skipEmptyLines();
        }
        const { commit, options } = this;
        const correspondence = options.headerCorrespondence || [];
        const header = commit.header ?? this.nextLine();
        let matches = null;
        if (header) {
            if (options.breakingHeaderPattern) {
                matches = header.match(options.breakingHeaderPattern);
            }
            if (!matches && options.headerPattern) {
                matches = header.match(options.headerPattern);
            }
        }
        if (header) {
            commit.header = header;
        }
        if (matches) {
            assignMatchedCorrespondence(commit, matches, correspondence);
        }
    }
    parseMeta() {
        const { options, commit } = this;
        if (!options.fieldPattern || !this.isLineAvailable()) {
            return false;
        }
        let matches;
        let field = null;
        let parsed = false;
        while (this.isLineAvailable()) {
            matches = this.currentLine().match(options.fieldPattern);
            if (matches) {
                field = matches[1] || null;
                this.nextLine();
                continue;
            }
            if (field) {
                parsed = true;
                commit[field] = appendLine(commit[field], this.currentLine());
                this.nextLine();
            }
            else {
                break;
            }
        }
        return parsed;
    }
    parseNotes() {
        const { regexes, commit } = this;
        if (!this.isLineAvailable()) {
            return false;
        }
        const matches = this.currentLine().match(regexes.notes);
        let isFooterToken;
        if (matches) {
            const note = {
                title: matches[1],
                text: matches[2]
            };
            commit.notes.push(note);
            commit.footer = appendLine(commit.footer, this.currentLine());
            this.nextLine();
            while (this.isLineAvailable()) {
                if (this.parseMeta()) {
                    return true;
                }
                if (this.parseNotes()) {
                    return true;
                }
                isFooterToken = regexes.footerToken.test(this.currentLine());
                commit.references.push(...this.parseReferences(this.currentLine(), isFooterToken));
                if (!isFooterToken) {
                    note.text = appendLine(note.text, this.currentLine());
                }
                commit.footer = appendLine(commit.footer, this.currentLine());
                this.nextLine();
                if (isFooterToken) {
                    break;
                }
            }
            return true;
        }
        return false;
    }
    parseBodyAndFooter(isBody) {
        const { commit, regexes } = this;
        if (!this.isLineAvailable()) {
            return isBody;
        }
        const isFooterToken = regexes.footerToken.test(this.currentLine());
        const isStillBody = !isFooterToken && isBody;
        commit.references.push(...this.parseReferences(this.currentLine(), isFooterToken));
        if (isStillBody) {
            commit.body = appendLine(commit.body, this.currentLine());
        }
        else {
            commit.footer = appendLine(commit.footer, this.currentLine());
        }
        this.nextLine();
        return isStillBody;
    }
    parseBreakingHeader() {
        const { commit, options } = this;
        if (!options.breakingHeaderPattern || commit.notes.length || !commit.header) {
            return;
        }
        const matches = commit.header.match(options.breakingHeaderPattern);
        if (matches) {
            commit.notes.push({
                title: 'BREAKING CHANGE',
                text: matches[3]
            });
        }
    }
    parseMentions(input) {
        const { commit, regexes } = this;
        let matches;
        for (;;) {
            matches = regexes.mentions.exec(input);
            if (!matches) {
                break;
            }
            commit.mentions.push(matches[1]);
        }
    }
    parseRevert(input) {
        const { commit, options } = this;
        const correspondence = options.revertCorrespondence || [];
        const matches = options.revertPattern
            ? input.match(options.revertPattern)
            : null;
        if (matches) {
            commit.revert = assignMatchedCorrespondence({}, matches, correspondence);
        }
    }
    cleanupCommit() {
        const { commit } = this;
        commit.body &&= trimNewLines(commit.body);
        commit.footer &&= trimNewLines(commit.footer);
        commit.notes.forEach((note) => {
            note.text = trimNewLines(note.text);
        });
        const referencesSet = new Set();
        commit.references = commit.references.filter((reference) => {
            const uid = `${reference.action} ${reference.raw}`.toLocaleLowerCase();
            const ok = !referencesSet.has(uid);
            if (ok) {
                referencesSet.add(uid);
            }
            return ok;
        });
    }
    /**
     * Parse commit message string into an object.
     * @param input - Commit message string.
     * @returns Commit object.
     */
    parse(input) {
        if (!input.trim()) {
            throw new TypeError('Expected a raw commit');
        }
        const { commentChar } = this.options;
        const commentFilter = getCommentFilter(commentChar);
        const rawLines = trimNewLines(input).split(/\r?\n/);
        const lines = commentChar
            ? truncateToScissor(rawLines, commentChar).filter(line => commentFilter(line) && gpgFilter(line))
            : rawLines.filter(line => gpgFilter(line));
        const commit = createCommitObject();
        this.lines = lines;
        this.lineIndex = 0;
        this.commit = commit;
        const isMergeCommit = this.parseMerge();
        this.parseHeader(isMergeCommit);
        if (commit.header) {
            commit.references = this.parseReferences(commit.header);
        }
        let isBody = true;
        while (this.isLineAvailable()) {
            this.parseMeta();
            if (this.parseNotes()) {
                isBody = false;
            }
            if (!this.parseBodyAndFooter(isBody)) {
                isBody = false;
            }
        }
        this.parseBreakingHeader();
        this.parseMentions(input);
        this.parseRevert(input);
        this.cleanupCommit();
        return commit;
    }
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoiQ29tbWl0UGFyc2VyLmpzIiwic291cmNlUm9vdCI6IiIsInNvdXJjZXMiOlsiLi4vc3JjL0NvbW1pdFBhcnNlci50cyJdLCJuYW1lcyI6W10sIm1hcHBpbmdzIjoiQUFPQSxPQUFPLEVBQUUsZ0JBQWdCLEVBQUUsTUFBTSxZQUFZLENBQUE7QUFDN0MsT0FBTyxFQUNMLFlBQVksRUFDWixVQUFVLEVBQ1YsZ0JBQWdCLEVBQ2hCLFNBQVMsRUFDVCxpQkFBaUIsRUFDakIsMkJBQTJCLEVBQzVCLE1BQU0sWUFBWSxDQUFBO0FBQ25CLE9BQU8sRUFBRSxjQUFjLEVBQUUsTUFBTSxjQUFjLENBQUE7QUFFN0M7Ozs7R0FJRztBQUNILE1BQU0sVUFBVSxrQkFBa0IsQ0FBQyxXQUFXLEdBQW9CLEVBQUU7SUFDbEUsa0pBQWtKO0lBQ2xKLE9BQU87UUFDTCxLQUFLLEVBQUUsSUFBSTtRQUNYLE1BQU0sRUFBRSxJQUFJO1FBQ1osTUFBTSxFQUFFLElBQUk7UUFDWixJQUFJLEVBQUUsSUFBSTtRQUNWLE1BQU0sRUFBRSxJQUFJO1FBQ1osS0FBSyxFQUFFLEVBQUU7UUFDVCxRQUFRLEVBQUUsRUFBRTtRQUNaLFVBQVUsRUFBRSxFQUFFO1FBQ2QsR0FBRyxXQUFXO0tBQ2YsQ0FBQTtBQUNILENBQUM7QUFFRDs7R0FFRztBQUNILE1BQU0sT0FBTyxZQUFZO0lBQ04sT0FBTyxDQUFlO0lBQ3RCLE9BQU8sQ0FBZTtJQUMvQixLQUFLLEdBQWEsRUFBRSxDQUFBO0lBQ3BCLFNBQVMsR0FBRyxDQUFDLENBQUE7SUFDYixNQUFNLEdBQUcsa0JBQWtCLEVBQUUsQ0FBQTtJQUVyQyxZQUFZLE9BQU8sR0FBa0IsRUFBRTtRQUNyQyxJQUFJLENBQUMsT0FBTyxHQUFHO1lBQ2IsR0FBRyxjQUFjO1lBQ2pCLEdBQUcsT0FBTztTQUNYLENBQUE7UUFDRCxJQUFJLENBQUMsT0FBTyxHQUFHLGdCQUFnQixDQUFDLElBQUksQ0FBQyxPQUFPLENBQUMsQ0FBQTtJQUMvQyxDQUFDO0lBRU8sV0FBVztRQUNq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