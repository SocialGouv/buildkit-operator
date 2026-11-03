import fs from 'fs/promises';
import { Readable } from 'stream';
import { ConventionalGitClient, packagePrefix } from '@conventional-changelog/git-client';
import { transformCommit, formatDate, writeChangelog } from 'conventional-changelog-writer';
import { createPresetLoader, loadPreset as defaultLoadPreset } from 'conventional-changelog-preset-loader';
import normalizePackageData from 'normalize-package-data';
import { findPackage } from 'fd-package-json';
import { parseHostedGitUrl } from '@simple-libs/hosted-git-info';
import { getHostOptions, guessNextTag, isUnreleasedVersion, versionTagRegex, defaultCommitTransform, bindLogNamespace } from './utils.js';
export { packagePrefix };
/**
 * Conventional changelog generator
 */
export class ConventionalChangelog {
    gitClient;
    params;
    constructor(cwdOrGitClient = process.cwd()) {
        this.gitClient = typeof cwdOrGitClient === 'string'
            ? new ConventionalGitClient(cwdOrGitClient)
            : cwdOrGitClient;
        this.params = Promise.resolve({
            options: {
                append: false,
                releaseCount: 1,
                formatDate,
                transformCommit: defaultCommitTransform
            },
            commits: {
                format: '%B%n-hash-%n%H%n-gitTags-%n%d%n-committerDate-%n%ci',
                merges: false
            }
        });
    }
    composeParams(params) {
        this.params = Promise.all([params, this.params]).then(([params, prevParams]) => ({
            options: {
                ...prevParams.options,
                ...params.options
            },
            context: {
                ...prevParams.context,
                ...params.context
            },
            tags: {
                ...prevParams.tags,
                ...params.tags
            },
            commits: {
                ...prevParams.commits,
                ...params.commits
            },
            parser: {
                ...prevParams.parser,
                ...params.parser
            },
            writer: {
                ...prevParams.writer,
                ...params.writer
            },
            repository: {
                ...prevParams.repository,
                ...params.repository
            },
            package: prevParams.package || params.package
        }));
    }
    async finalizeContext(semverTags, hostOptions) {
        const { options, package: pkg, repository, context } = await this.params;
        const finalContext = {
            packageData: pkg,
            version: pkg?.version,
            gitSemverTags: semverTags,
            ...context
        };
        if (repository) {
            finalContext.repoUrl ||= repository.url;
            finalContext.host ||= repository.host;
            finalContext.owner ||= repository.owner;
            finalContext.repository ||= repository.project;
        }
        if (hostOptions) {
            finalContext.issue ||= hostOptions.issue;
            finalContext.commit ||= hostOptions.commit;
        }
        if (isUnreleasedVersion(semverTags, finalContext.version) && options.outputUnreleased) {
            finalContext.version = 'Unreleased';
        }
        return finalContext;
    }
    async finalizeWriterOptions(semverTags, version) {
        const { options, tags, writer } = await this.params;
        let doFlush = options.outputUnreleased;
        if (isUnreleasedVersion(semverTags, version) && !doFlush) {
            doFlush = false;
        }
        else if (typeof doFlush !== 'boolean') {
            doFlush = true;
        }
        const finalOptions = {
            finalizeContext(context, _writerOpts, _filteredCommits, keyCommit, originalCommits) {
                const [firstCommit] = originalCommits;
                const lastCommit = originalCommits[originalCommits.length - 1];
                const firstCommitHash = firstCommit ? firstCommit.hash : null;
                const lastCommitHash = lastCommit ? lastCommit.hash : null;
                if ((!context.currentTag || !context.previousTag) && keyCommit) {
                    const matches = keyCommit.gitTags?.match(versionTagRegex);
                    const { currentTag } = context;
                    context.currentTag = currentTag || matches?.[1]; // currentTag || matches ? matches[1] : null
                    const index = context.currentTag
                        ? semverTags.indexOf(context.currentTag)
                        : -1;
                    // if `keyCommit.gitTags` is not a semver
                    if (index === -1) {
                        context.currentTag = currentTag || null;
                    }
                    else {
                        const previousTag = semverTags[index + 1];
                        context.previousTag = previousTag;
                        if (!previousTag) {
                            if (options.append) {
                                context.previousTag ||= firstCommitHash;
                            }
                            else {
                                context.previousTag ||= lastCommitHash;
                            }
                        }
                    }
                }
                else {
                    context.previousTag ||= semverTags[0];
                    if (context.version === 'Unreleased') {
                        if (options.append) {
                            context.currentTag ||= lastCommitHash;
                        }
                        else {
                            context.currentTag ||= firstCommitHash;
                        }
                    }
                    else if (!context.currentTag) {
                        if (tags?.prefix) {
                            context.currentTag = tags.prefix + (context.version || '');
                        }
                        else {
                            context.currentTag = guessNextTag(semverTags[0], context.version);
                        }
                    }
                }
                if (typeof context.linkCompare !== 'boolean' && context.previousTag && context.currentTag) {
                    context.linkCompare = true;
                }
                return context;
            },
            reverse: options.append,
            doFlush,
            ...writer
        };
        if (!finalOptions.debug && options.debug) {
            finalOptions.debug = bindLogNamespace('writer', options.debug);
        }
        return finalOptions;
    }
    async getSemverTags() {
        const { gitClient } = this;
        const { tags: params } = await this.params;
        const tags = [];
        for await (const tag of gitClient.getSemverTags(params)) {
            tags.push(tag);
        }
        return tags;
    }
    async *getCommits(semverTags, hostOptions) {
        const { gitClient } = this;
        const { options, commits, parser } = await this.params;
        const { reset, releaseCount } = options;
        const params = {
            from: reset
                ? undefined
                : releaseCount
                    ? semverTags[releaseCount - 1]
                    : undefined,
            ...commits
        };
        const parserParams = {
            ...parser
        };
        if (!parserParams.warn && options.warn) {
            parserParams.warn = bindLogNamespace('parser', options.warn);
        }
        if (options.append) {
            params.reverse = true;
        }
        if (hostOptions?.referenceActions && !parserParams.referenceActions?.length) {
            parserParams.referenceActions = hostOptions.referenceActions;
        }
        if (hostOptions?.issuePrefixes && !parserParams.issuePrefixes?.length) {
            parserParams.issuePrefixes = hostOptions.issuePrefixes;
        }
        try {
            await gitClient.verify('HEAD');
            let reverseTags = semverTags.slice().reverse();
            reverseTags.push('HEAD');
            if (params.from) {
                if (reverseTags.includes(params.from)) {
                    reverseTags = reverseTags.slice(reverseTags.indexOf(params.from));
                }
                else {
                    reverseTags = [params.from, 'HEAD'];
                }
            }
            else {
                reverseTags.unshift('');
            }
            const streams = [];
            for (let i = 1, len = reverseTags.length; i < len; i++) {
                streams.push(gitClient.getCommits({
                    ...params,
                    from: reverseTags[i - 1],
                    to: reverseTags[i]
                }, parserParams));
            }
            if (!params.reverse) {
                streams.reverse();
            }
            for (const stream of streams) {
                yield* stream;
            }
        }
        catch {
            yield* gitClient.getCommits(params, parserParams);
        }
    }
    async *transformCommits(commits) {
        const params = await this.params;
        const { transformCommit: transform } = params.options;
        let transformed;
        for await (const commit of commits) {
            transformed = await transformCommit(commit, transform, params);
            if (transformed) {
                yield transformed;
            }
        }
    }
    async getPackageJson(pkgPath, transform) {
        const { gitClient } = this;
        let pkg;
        if (pkgPath) {
            pkg = JSON.parse(await fs.readFile(pkgPath, 'utf-8'));
        }
        else {
            pkg = (await findPackage(gitClient.cwd) || {});
        }
        normalizePackageData(pkg);
        if (!pkg.repository?.url) {
            try {
                const repoUrl = await gitClient.getConfig('remote.origin.url');
                if (repoUrl) {
                    pkg.repository = {
                        ...pkg.repository,
                        url: repoUrl
                    };
                }
            }
            catch { }
        }
        if (transform) {
            pkg = transform(pkg);
        }
        const result = {
            package: pkg
        };
        const repositoryURL = (pkg.repository?.url || pkg.repository);
        if (repositoryURL) {
            result.repository = parseHostedGitUrl(repositoryURL);
        }
        return result;
    }
    /**
     * Load configs from a preset
     * @param preset
     * @param loader - Preset module loader, if not provided, will use default loader
     * @returns this
     */
    loadPreset(preset, loader) {
        const loadPreset = loader ? createPresetLoader(loader) : defaultLoadPreset;
        const config = loadPreset(preset).then((config) => {
            if (!config) {
                throw Error('Preset is not loaded or have incorrect exports');
            }
            return config;
        });
        this.composeParams(config);
        return this;
    }
    /**
     * Set the config directly
     * @param config - Config object
     * @returns this
     */
    config(config) {
        this.composeParams(config);
        return this;
    }
    readPackage(pathOrTransform, maybeTransform) {
        const [pkgPath, transform] = typeof pathOrTransform === 'string'
            ? [pathOrTransform, maybeTransform]
            : [undefined, pathOrTransform];
        this.composeParams(this.getPackageJson(pkgPath, transform));
        return this;
    }
    /**
     * Set package.json data
     * @param pkg - Package.json data
     * @returns this
     */
    package(pkg) {
        this.composeParams({
            package: pkg
        });
        return this;
    }
    /**
     * Read repository info from the current git repository
     * @returns this
     */
    readRepository() {
        this.composeParams(this.gitClient.getConfig('remote.origin.url').then(repository => ({
            repository: parseHostedGitUrl(repository)
        })));
        return this;
    }
    /**
     * Set repository info
     * @param infoOrGitUrl - Hosted git info or git url
     * @returns this
     */
    repository(infoOrGitUrl) {
        const info = typeof infoOrGitUrl === 'string'
            ? parseHostedGitUrl(infoOrGitUrl)
            : infoOrGitUrl;
        this.composeParams({
            repository: info
        });
        return this;
    }
    /**
     * Set conventional-changelog options
     * @param options - Generator options
     * @returns this
     */
    options(options) {
        this.composeParams({
            options
        });
        return this;
    }
    /**
     * Set writer context data
     * @param context - Writer context data
     * @returns this
     */
    context(context) {
        this.composeParams({
            context
        });
        return this;
    }
    /**
     * Set params to get semver tags
     * @param params - Params to get the last semver tag
     * @returns this
     */
    tags(params) {
        this.composeParams({
            tags: params
        });
        return this;
    }
    /**
     * Set params to get commits
     * @param params - Params to get commits since last release
     * @param parserOptions - Parser options
     * @returns this
     */
    commits(params, parserOptions) {
        this.composeParams({
            commits: params,
            parser: parserOptions
        });
        return this;
    }
    /**
     * Set writer options
     * @param params - Writer options
     * @returns this
     */
    writer(params) {
        this.composeParams({
            writer: params
        });
        return this;
    }
    async *write(includeDetails) {
        const { gitClient } = this;
        const { options, repository, context } = await this.params;
        const hostOptions = getHostOptions(repository, context);
        if (!gitClient.debug && options.debug) {
            gitClient.debug = bindLogNamespace('git-client', options.debug);
        }
        if (!hostOptions && options.warn) {
            options.warn('core', `Host is not supported: ${context?.host || repository?.host}`);
        }
        const semverTags = await this.getSemverTags();
        const finalContext = await this.finalizeContext(semverTags, hostOptions);
        const writerOptions = await this.finalizeWriterOptions(semverTags, finalContext.version);
        const commits = this.getCommits(semverTags, hostOptions);
        const transformedCommits = this.transformCommits(commits);
        const changelogWriter = writeChangelog(finalContext, writerOptions, includeDetails);
        yield* changelogWriter(transformedCommits);
    }
    /**
     * Generate changelog to stream
     * @param includeDetails - Generate data objects instead of strings
     * @returns Changelog stream
     */
    writeStream(includeDetails) {
        return Readable.from(this.write(includeDetails));
    }
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoiQ29udmVudGlvbmFsQ2hhbmdlbG9nLmpzIiwic291cmNlUm9vdCI6IiIsInNvdXJjZXMiOlsiLi4vc3JjL0NvbnZlbnRpb25hbENoYW5nZWxvZy50cyJdLCJuYW1lcyI6W10sIm1hcHBpbmdzIjoiQUFBQSxPQUFPLEVBQUUsTUFBTSxhQUFhLENBQUE7QUFDNUIsT0FBTyxFQUFFLFFBQVEsRUFBRSxNQUFNLFFBQVEsQ0FBQTtBQUtqQyxPQUFPLEVBR0wscUJBQXFCLEVBQ3JCLGFBQWEsRUFDZCxNQUFNLG9DQUFvQyxDQUFBO0FBQzNDLE9BQU8sRUFJTCxlQUFlLEVBQ2YsVUFBVSxFQUNWLGNBQWMsRUFDZixNQUFNLCtCQUErQixDQUFBO0FBQ3RDLE9BQU8sRUFJTCxrQkFBa0IsRUFDbEIsVUFBVSxJQUFJLGlCQUFpQixFQUNoQyxNQUFNLHNDQUFzQyxDQUFBO0FBQzdDLE9BQU8sb0JBQW9CLE1BQU0sd0JBQXdCLENBQUE7QUFDekQsT0FBTyxFQUFFLFdBQVcsRUFBRSxNQUFNLGlCQUFpQixDQUFBO0FBQzdDLE9BQU8sRUFFTCxpQkFBaUIsRUFDbEIsTUFBTSw4QkFBOEIsQ0FBQTtBQVVyQyxPQUFPLEVBQ0wsY0FBYyxFQUNkLFlBQVksRUFDWixtQkFBbUIsRUFDbkIsZUFBZSxFQUNmLHNCQUFzQixFQUN0QixnQkFBZ0IsRUFDakIsTUFBTSxZQUFZLENBQUE7QUFFbkIsT0FBTyxFQUFFLGFBQWEsRUFBRSxDQUFBO0FBRXhCOztHQUVHO0FBQ0gsTUFBTSxPQUFPLHFCQUFxQjtJQUNmLFNBQVMsQ0FBdUI7SUFDekMsTUFBTSxDQUFpQjtJQUUvQixZQUFZLGNBQWMsR0FBbUMsT0FBTyxDQUFDLEdBQUcsRUFBRTtRQUN4RSxJQUFJLENBQUMsU0FBUyxHQUFHLE9BQU8sY0FBYyxLQUFLLFFBQVE7WUFDakQsQ0FBQyxDQUFDLElBQUkscUJBQXFCLENBQUMsY0FBYyxDQUFDO1lBQzNDLENBQUMsQ0FBQyxjQUFjLENBQUE7UUFFbEIsSUFBSSxDQUFDLE1BQU0sR0FBRyxPQUFP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